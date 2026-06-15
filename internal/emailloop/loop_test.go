package emailloop

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	corestore "github.com/danmestas/notify/internal/store"
)

const inboundSecret = "s3cr3t-shared"

// loopFixture is a fully wired email loop over a real store, with a spy
// responder that records dispatches AND performs the real conversation append
// (Upsert + Append) so a test can assert both that the agent responder fired
// and that the conversation gained the inbound turn — without needing NATS.
type loopFixture struct {
	st     *corestore.Store
	ags    *agentstore.Store
	svc    *Service
	minter *Minter

	mu         sync.Mutex
	dispatches []InboundTurn
}

func newLoopFixture(t *testing.T, resolver EmailResolver) *loopFixture {
	t.Helper()
	ctx := context.Background()
	st, err := corestore.Open(ctx, filepath.Join(t.TempDir(), "loop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ags := agentstore.New(st)
	minter := NewMinter(testKey())

	f := &loopFixture{st: st, ags: ags, minter: minter}

	// The compose func IS the real agent-responder path, reduced to the store
	// writes ComposeMessage performs (upsert conversation, append the user
	// activity). It records the dispatch so the test can assert the responder
	// fired. The dagnats turn run is omitted (it needs NATS); the durable
	// transcript mutation it would precede is exactly what we assert.
	compose := func(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (string, error) {
		f.mu.Lock()
		f.dispatches = append(f.dispatches, InboundTurn{
			Scope: scope, AgentIdentifier: agentIdentifier,
			ThreadID: conversationID, SubscriberID: subscriberID, Text: text,
		})
		f.mu.Unlock()

		agent, err := ags.Agents().GetByIdentifier(ctx, scope, agentIdentifier)
		if err != nil {
			return "", err
		}
		conv, err := ags.Conversations().Upsert(ctx, scope, agentdomain.Conversation{
			AgentID: agent.ID, IntegrationID: "", PlatformThreadID: conversationID, SubscriberID: subscriberID,
		})
		if err != nil {
			return "", err
		}
		content, _ := json.Marshal(map[string][]agentdomain.Part{"parts": {{Text: text}}})
		if _, err := ags.Activities().Append(ctx, scope, agentdomain.Activity{
			ConversationID: conv.ID, Role: "user", Content: content,
		}); err != nil {
			return "", err
		}
		return conv.PlatformThreadID, nil
	}

	f.svc = New(st, ags, minter, resolver, NewAgentResponder(compose), inboundSecret, slog.Default())
	return f
}

func (f *loopFixture) dispatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatches)
}

// armScope bootstraps a scope with a verified inbound domain, an agent, and an
// armed email thread (conversation). Returns the scope, the inbox thread id,
// and the subscriber id.
func (f *loopFixture) armScope(t *testing.T, ctx context.Context, org, env, agentID, domainName, subEmail string) (domain.Scope, string, string) {
	t.Helper()
	scope := domain.Scope{OrgID: org, EnvID: env}

	if _, err := f.st.InboundDomains().Create(ctx, scope, domainName, "mx.notify.test", 10); err != nil {
		t.Fatalf("inbound domain create: %v", err)
	}
	if err := f.st.InboundDomains().SetStatus(ctx, scope, domainName, "verified", true); err != nil {
		t.Fatalf("inbound domain verify: %v", err)
	}
	if _, err := f.ags.Agents().Create(ctx, scope, agentdomain.Agent{
		Identifier: agentID, Name: agentID, Runtime: agentdomain.RuntimeMesh, Active: true,
	}); err != nil {
		t.Fatalf("agent create: %v", err)
	}
	threadID := corestore.NewULID()
	subID := "sub-" + env
	if err := f.svc.ArmEmailThread(ctx, scope, agentID, threadID, subID, subEmail, domainName); err != nil {
		t.Fatalf("ArmEmailThread: %v", err)
	}
	return scope, threadID, subID
}

// postInbound drives the inbound handler and returns the HTTP status plus the
// decoded acceptedResponse status field (empty when the body is not a JSON
// accepted-response, e.g. on a 401). It reads and closes the body so callers
// never hold an open response.
func postInbound(t *testing.T, h http.HandlerFunc, secret string, payload InboundEmail) (int, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/inbound/email", strings.NewReader(string(body)))
	if secret != "" {
		req.Header.Set("X-Inbound-Secret", secret)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var out acceptedResponse
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out.Status
}

// TestEmailLoop_OutboundReplyTo_ThenInboundThreadsAndDispatches is the core
// proof: arm an email-origin conversation, assert the outbound reply-to carries
// reply+<token>@<domain>, POST that token back as a simulated inbound email, and
// assert (a) the conversation gains a new inbound turn with the email body and
// (b) the agent responder was dispatched.
func TestEmailLoop_OutboundReplyTo_ThenInboundThreadsAndDispatches(t *testing.T) {
	ctx := context.Background()
	f := newLoopFixture(t, nil)
	scope, threadID, subID := f.armScope(t, ctx, "org-a", "env-a", "support", "inbound-a.test", "alice@customer.test")

	// (1) Outbound arming: the reply-to address the outbound send would carry.
	addr, ok := f.svc.ReplyToAddress(ctx, scope, threadID, subID)
	if !ok {
		t.Fatal("ReplyToAddress: ok=false, want a tokenized reply-to (verified domain + conversation)")
	}
	if !strings.HasPrefix(addr, "reply+") || !strings.HasSuffix(addr, "@inbound-a.test") {
		t.Fatalf("reply-to address = %q, want reply+<token>@inbound-a.test", addr)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(addr, "reply+"), "@inbound-a.test")
	// The token round-trips to the conversation it correlates.
	claims, err := f.minter.Verify(token)
	if err != nil {
		t.Fatalf("token from reply-to does not verify: %v", err)
	}
	if claims.ThreadID != threadID || claims.EnvID != "env-a" {
		t.Fatalf("token claims = %+v, want thread=%s env=env-a", claims, threadID)
	}

	// (2) Inbound: a customer reply arrives at the reply-to mailbox. The body
	// carries quoted history that should be stripped.
	status, result := postInbound(t, f.svc.HandleInbound, inboundSecret, InboundEmail{
		From:       "alice@customer.test",
		To:         addr,
		EnvelopeTo: addr,
		Subject:    "Re: your ticket",
		Text:       "Yes please go ahead.\n\nOn Mon, Jan 1, 2026 Bob wrote:\n> original message",
	})
	if status != http.StatusOK {
		t.Fatalf("inbound status = %d, want 200", status)
	}
	if result != "threaded" {
		t.Fatalf("inbound result = %q, want threaded", result)
	}

	// (b) The agent responder was dispatched with the resolved scope/agent/body.
	if f.dispatchCount() != 1 {
		t.Fatalf("dispatch count = %d, want 1", f.dispatchCount())
	}
	f.mu.Lock()
	d := f.dispatches[0]
	f.mu.Unlock()
	if d.AgentIdentifier != "support" || d.ThreadID != threadID || d.SubscriberID != subID {
		t.Errorf("dispatch turn = %+v, want agent=support thread=%s sub=%s", d, threadID, subID)
	}
	if d.Text != "Yes please go ahead." {
		t.Errorf("dispatched body = %q, want quoted history stripped to %q", d.Text, "Yes please go ahead.")
	}

	// (a) The conversation gained the inbound turn (the durable transcript).
	conv, err := f.ags.Conversations().GetByThread(ctx, scope, agentID(t, ctx, f, scope, "support"), "", threadID)
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	acts, err := f.ags.Activities().History(ctx, scope, conv.ID, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("conversation activities = %d, want 1 inbound turn", len(acts))
	}
	if acts[0].Role != "user" || !strings.Contains(string(acts[0].Content), "Yes please go ahead.") {
		t.Errorf("inbound turn = role %q content %s, want user turn with the email body", acts[0].Role, acts[0].Content)
	}
}

// TestEmailLoop_InvalidTokenDropped: an unparseable/forged token is dropped
// with 200 and no conversation mutation.
func TestEmailLoop_InvalidTokenDropped(t *testing.T) {
	ctx := context.Background()
	f := newLoopFixture(t, nil)
	f.armScope(t, ctx, "org-a", "env-a", "support", "inbound-a.test", "alice@customer.test")

	status, result := postInbound(t, f.svc.HandleInbound, inboundSecret, InboundEmail{
		From: "mallory@evil.test", To: "reply+not-a-real-token@inbound-a.test",
		EnvelopeTo: "reply+not-a-real-token@inbound-a.test", Text: "let me in",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (drop)", status)
	}
	if result != "invalid_token" {
		t.Fatalf("result = %q, want invalid_token", result)
	}
	if f.dispatchCount() != 0 {
		t.Fatalf("dispatch count = %d, want 0 (no mutation on invalid token)", f.dispatchCount())
	}
}

// TestEmailLoop_MissingSecretRejected: no/wrong X-Inbound-Secret is a 401.
func TestEmailLoop_MissingSecretRejected(t *testing.T) {
	ctx := context.Background()
	f := newLoopFixture(t, nil)
	scope, threadID, subID := f.armScope(t, ctx, "org-a", "env-a", "support", "inbound-a.test", "alice@customer.test")
	addr, _ := f.svc.ReplyToAddress(ctx, scope, threadID, subID)

	// No secret header.
	status, _ := postInbound(t, f.svc.HandleInbound, "", InboundEmail{To: addr, EnvelopeTo: addr, Text: "hi"})
	if status != http.StatusUnauthorized {
		t.Fatalf("missing-secret status = %d, want 401", status)
	}
	// Wrong secret.
	status, _ = postInbound(t, f.svc.HandleInbound, "wrong-secret", InboundEmail{To: addr, EnvelopeTo: addr, Text: "hi"})
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong-secret status = %d, want 401", status)
	}
	if f.dispatchCount() != 0 {
		t.Fatalf("dispatch count = %d, want 0", f.dispatchCount())
	}
}

// TestEmailLoop_ScopeIsolation: a token minted for env-a cannot thread into
// env-b. The token carries env-a's scope, so even posted at env-b's domain it
// resolves to env-a's binding — and crucially the domain check rejects an
// env-a token presented at a host verified only for env-b.
func TestEmailLoop_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	f := newLoopFixture(t, nil)
	scopeA, threadA, subA := f.armScope(t, ctx, "org-a", "env-a", "support", "inbound-a.test", "alice@a.test")
	f.armScope(t, ctx, "org-b", "env-b", "support", "inbound-b.test", "bob@b.test")

	// Mint a token for env-a's conversation, but present it at env-b's domain.
	tokenA := f.minter.Mint(scopeA, threadA, subA)
	crossAddr := "reply+" + tokenA + "@inbound-b.test"
	status, result := postInbound(t, f.svc.HandleInbound, inboundSecret, InboundEmail{
		To: crossAddr, EnvelopeTo: crossAddr, Text: "cross-tenant attempt",
	})
	if status != http.StatusOK {
		t.Fatalf("cross-scope status = %d, want 200 (drop)", status)
	}
	if result != "domain_unverified" {
		t.Fatalf("cross-scope result = %q, want domain_unverified (env-a token rejected at env-b host)", result)
	}
	if f.dispatchCount() != 0 {
		t.Fatalf("dispatch count = %d, want 0 (no cross-tenant threading)", f.dispatchCount())
	}
}

// agentID is a tiny helper to resolve an agent's id within a scope.
func agentID(t *testing.T, ctx context.Context, f *loopFixture, scope domain.Scope, identifier string) string {
	t.Helper()
	a, err := f.ags.Agents().GetByIdentifier(ctx, scope, identifier)
	if err != nil {
		t.Fatalf("GetByIdentifier: %v", err)
	}
	return a.ID
}

// TestEmailLoop_CloseTheLoop_AgentReplyMailedBack proves the closing leg: the
// ReplyHook mails the agent's reply back out with the same tokenized reply-to.
func TestEmailLoop_CloseTheLoop_AgentReplyMailedBack(t *testing.T) {
	ctx := context.Background()
	cap := providers.NewCapture()
	resolver := captureResolver{cap: cap, from: "support@notify.test"}
	f := newLoopFixture(t, resolver)
	scope, threadID, _ := f.armScope(t, ctx, "org-a", "env-a", "support", "inbound-a.test", "alice@customer.test")

	// Simulate ingress calling the hook after persisting an agent reply.
	conv := agentdomain.Conversation{
		OrgID: scope.OrgID, EnvID: scope.EnvID, PlatformThreadID: threadID,
	}
	f.svc.ReplyHook(ctx, scope, conv, "Here is your answer.")

	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("captured sends = %d, want 1 (agent reply mailed back)", len(sent))
	}
	msg := sent[0]
	if msg.To != "alice@customer.test" {
		t.Errorf("To = %q, want alice@customer.test", msg.To)
	}
	if msg.Text != "Here is your answer." {
		t.Errorf("Text = %q, want the agent reply", msg.Text)
	}
	if !strings.HasPrefix(msg.ReplyTo, "reply+") || !strings.HasSuffix(msg.ReplyTo, "@inbound-a.test") {
		t.Fatalf("closing Reply-To = %q, want reply+<token>@inbound-a.test", msg.ReplyTo)
	}
	// The closing reply-to token re-correlates to the same conversation.
	tok := strings.TrimSuffix(strings.TrimPrefix(msg.ReplyTo, "reply+"), "@inbound-a.test")
	claims, err := f.minter.Verify(tok)
	if err != nil || claims.ThreadID != threadID {
		t.Fatalf("closing token claims = %+v err=%v, want thread=%s", claims, err, threadID)
	}
}

// captureResolver is an EmailResolver that hands back a providers.Capture so the
// closing send can be asserted without a real provider.
type captureResolver struct {
	cap  *providers.Capture
	from string
}

func (r captureResolver) ResolveEmail(_ context.Context, _ domain.Scope) (providers.EmailProvider, string, string, string, error) {
	return r.cap, r.from, "capture", "int-cap", nil
}
