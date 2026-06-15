package emailloop

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	corestore "github.com/danmestas/notify/internal/store"
)

// EmailResolver resolves the active email provider and From address for a
// scope. connectivity.Service satisfies it structurally (emailloop does not
// import connectivity). Mirrors delivery.EmailResolver's return shape.
type EmailResolver interface {
	ResolveEmail(ctx context.Context, scope domain.Scope) (providers.EmailProvider, string, string, string, error)
}

// Service is the email two-way loop. It owns the reply-token minter, the
// email-thread correlation store, the verified-inbound-domain lookup, the
// inbound-parse HTTP handler, the responder seam, and the outbound reply path
// that closes the loop.
type Service struct {
	core      *corestore.Store
	agents    *agentstore.Store
	minter    *Minter
	resolver  EmailResolver
	responder Responder
	secret    string
	log       *slog.Logger
}

// New builds the Service. secret is the shared secret the inbound mail
// forwarder must present (X-Inbound-Secret); an empty secret disables the
// inbound endpoint (it answers 503), so a misconfigured deploy fails closed
// rather than accepting unauthenticated inbound mail. responder is the default
// turn handler (typically an AgentResponder); a nil resolver disables the
// outbound close (the inbound leg still works).
func New(
	core *corestore.Store,
	agents *agentstore.Store,
	minter *Minter,
	resolver EmailResolver,
	responder Responder,
	secret string,
	log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		core:      core,
		agents:    agents,
		minter:    minter,
		resolver:  resolver,
		responder: responder,
		secret:    secret,
		log:       log,
	}
}

// ArmEmailThread records (or refreshes) the email-origin binding for a
// conversation so the loop knows where to mail the agent's reply and which
// agent responds. It is called when an email-origin conversation is started
// (e.g. by the first inbound email, or by an operator wiring a workflow's email
// step to an agent). threadID is the conversation's inbox platform_thread_id.
func (s *Service) ArmEmailThread(ctx context.Context, scope domain.Scope, agentIdentifier, threadID, subscriberID, subscriberEmail, inboundDomain string) error {
	_, err := s.core.EmailThreads().Upsert(ctx, scope, corestore.EmailThread{
		AgentIdentifier:  agentIdentifier,
		PlatformThreadID: threadID,
		SubscriberID:     subscriberID,
		SubscriberEmail:  subscriberEmail,
		InboundDomain:    inboundDomain,
	})
	return err
}

// ReplyToAddress returns the tokenized reply-to mailbox for a conversation send
// and whether it should be set. The trigger rule: a reply-to is armed only when
// a verified inbound domain exists for the scope AND the send is tied to a
// conversation thread + subscriber. The returned address is
// reply+<token>@<inbound-domain>.
//
// Callers (the outbound email path) set msg.ReplyTo to the returned address
// only when ok is true.
func (s *Service) ReplyToAddress(ctx context.Context, scope domain.Scope, threadID, subscriberID string) (string, bool) {
	if threadID == "" || subscriberID == "" {
		return "", false
	}
	dom, ok := s.verifiedInboundDomain(ctx, scope)
	if !ok {
		return "", false
	}
	token := s.minter.Mint(scope, threadID, subscriberID)
	return "reply+" + token + "@" + dom, true
}

// verifiedInboundDomain returns the first verified inbound domain for scope,
// used by the outbound arming path which just needs a domain to mint a
// reply-to against. The InboundDomainRepo has no "get verified" method, so we
// page and filter.
func (s *Service) verifiedInboundDomain(ctx context.Context, scope domain.Scope) (string, bool) {
	for _, name := range s.verifiedInboundDomains(ctx, scope) {
		return name, true
	}
	return "", false
}

// isVerifiedInboundDomain reports whether host is a verified inbound domain for
// scope. Used by the inbound path to bind the recipient host to the token's
// tenant; membership (not first-only) so a scope with several verified domains
// accepts a reply at any of them.
func (s *Service) isVerifiedInboundDomain(ctx context.Context, scope domain.Scope, host string) bool {
	host = strings.ToLower(host)
	for _, name := range s.verifiedInboundDomains(ctx, scope) {
		if name == host {
			return true
		}
	}
	return false
}

// verifiedInboundDomains lists the lowercased names of every verified inbound
// domain for scope.
func (s *Service) verifiedInboundDomains(ctx context.Context, scope domain.Scope) []string {
	items, _, _, err := s.core.InboundDomains().List(ctx, scope, "", "", 200)
	if err != nil {
		s.log.Warn("emailloop: list inbound domains", "err", err)
		return nil
	}
	var out []string
	for _, d := range items {
		if d.Status == "verified" {
			out = append(out, strings.ToLower(d.Name))
		}
	}
	return out
}

// HandleInbound is the POST /v1/inbound/email handler. It is mounted OUTSIDE
// the API-key middleware because a mail forwarder has no API key; the shared
// secret in X-Inbound-Secret is the authentication.
//
// Flow: verify secret → decode payload → extract+verify token → confirm the
// token's inbound domain is still verified for the token's scope → thread the
// body onto the conversation via the responder. An unknown/invalid token is a
// 200-but-drop (so the mail provider does not retry-loop), logged. A missing or
// wrong secret is a 401.
func (s *Service) HandleInbound(w http.ResponseWriter, r *http.Request) {
	if s.secret == "" {
		// Fail closed: without a configured secret we cannot authenticate the
		// forwarder, so the endpoint is disabled rather than open.
		http.Error(w, "inbound email not configured", http.StatusServiceUnavailable)
		return
	}
	presented := r.Header.Get("X-Inbound-Secret")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var payload InboundEmail
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	token, host, ok := extractToken(payload.recipient())
	if !ok {
		// Not a reply mailbox: nothing to thread. Drop quietly (200) so the
		// forwarder treats it as accepted.
		s.log.Info("emailloop: inbound mail with no reply token, dropped", "to", payload.recipient())
		writeAccepted(w, "no_token")
		return
	}

	claims, err := s.minter.Verify(token)
	if err != nil {
		s.log.Warn("emailloop: inbound token verify failed, dropped", "err", err)
		writeAccepted(w, "invalid_token")
		return
	}
	scope := domain.Scope{OrgID: claims.OrgID, EnvID: claims.EnvID}

	// Confirm the inbound domain in the address is still a verified domain for
	// the token's scope. This closes a window where a token was minted against
	// a domain that has since been removed/unverified, and binds the address
	// host to the tenant rather than trusting the token alone.
	if !s.isVerifiedInboundDomain(ctx(r), scope, host) {
		s.log.Warn("emailloop: inbound domain not verified for scope, dropped",
			"host", host, "org", scope.OrgID, "env", scope.EnvID)
		writeAccepted(w, "domain_unverified")
		return
	}

	// Resolve the email-thread binding to learn the responder agent. The token
	// carries the thread id; the binding carries the agent. A missing binding
	// means the thread was never armed (e.g. token replayed after the binding
	// was pruned) — drop.
	binding, err := s.core.EmailThreads().Get(ctx(r), scope, claims.ThreadID)
	if err != nil {
		s.log.Warn("emailloop: no email-thread binding for token, dropped",
			"thread", claims.ThreadID, "err", err)
		writeAccepted(w, "no_binding")
		return
	}

	body := bestBody(payload)
	if body == "" {
		s.log.Info("emailloop: inbound mail had empty body, dropped", "thread", claims.ThreadID)
		writeAccepted(w, "empty_body")
		return
	}

	turn := InboundTurn{
		Scope:           scope,
		ThreadID:        claims.ThreadID,
		SubscriberID:    claims.SubscriberID,
		AgentIdentifier: binding.AgentIdentifier,
		Text:            body,
	}
	if _, err := s.responder.Respond(ctx(r), turn); err != nil {
		// A responder failure IS worth a non-2xx: the forwarder retry gives the
		// turn another chance (the compose primitive is idempotent on the
		// activity it appends per delivery). Store-not-found (bad agent) is a
		// permanent drop instead.
		if errors.Is(err, agentstore.ErrNotFound) {
			s.log.Warn("emailloop: responder agent not found, dropped",
				"agent", binding.AgentIdentifier, "err", err)
			writeAccepted(w, "agent_not_found")
			return
		}
		s.log.Error("emailloop: responder dispatch failed", "err", err)
		http.Error(w, "responder error", http.StatusInternalServerError)
		return
	}
	writeAccepted(w, "threaded")
}

// ReplyHook is the close-the-loop seam ingress calls after persisting an agent
// reply. When the conversation thread has an email-origin binding, the reply is
// mailed back to the subscriber with the same tokenized Reply-To so the thread
// continues. A thread with no binding (a pure inbox conversation) is a no-op.
//
// It is best-effort: a send failure is logged but never propagated, because the
// reply is already durably persisted as the in_app activity — failing the turn
// would re-run the agent and duplicate the answer.
func (s *Service) ReplyHook(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
	binding, err := s.core.EmailThreads().Get(ctx, scope, conv.PlatformThreadID)
	if err != nil {
		// No email binding for this thread: not an email-origin conversation.
		return
	}
	if s.resolver == nil {
		s.log.Warn("emailloop: reply hook fired but no email resolver configured",
			"thread", conv.PlatformThreadID)
		return
	}
	provider, from, _, _, err := s.resolver.ResolveEmail(ctx, scope)
	if err != nil {
		s.log.Error("emailloop: resolve email for reply", "err", err, "thread", conv.PlatformThreadID)
		return
	}
	replyTo := "reply+" + s.minter.Mint(scope, conv.PlatformThreadID, binding.SubscriberID) + "@" + binding.InboundDomain
	if _, err := provider.Send(ctx, providers.EmailMessage{
		From:    from,
		To:      binding.SubscriberEmail,
		Subject: "Re: your message",
		Text:    text,
		ReplyTo: replyTo,
	}); err != nil {
		s.log.Error("emailloop: send agent reply email", "err", err, "thread", conv.PlatformThreadID)
		return
	}
	s.log.Info("emailloop: agent reply mailed back", "thread", conv.PlatformThreadID, "to", binding.SubscriberEmail)
}

// acceptedResponse is the inbound-parse 200 body: a small status the forwarder
// can log. Every drop path returns 200 with a reason so the mail provider does
// not retry-loop on mail notify intentionally ignores.
type acceptedResponse struct {
	Status string `json:"status"`
}

func writeAccepted(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(acceptedResponse{Status: status})
}

// ctx is a tiny helper so the handler reads the request context once per use
// without repeating r.Context().
func ctx(r *http.Request) context.Context { return r.Context() }
