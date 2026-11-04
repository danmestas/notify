// Package a2areceiver lets a live harness answer an agent's turns over a
// WebSocket, in place of the agent's configured runtime.
//
// A harness dials `GET /a2a/{agentId}/receive` with a receiver key
// (`Authorization: Bearer a2arx_...`), a credential bound to that one agent.
// While it is connected, every turn of the agent, whatever channel it arrived
// on, is pushed to it; when it disconnects (or stops answering pings) turns go
// back to the configured runtime, and a turn that was in flight on the socket
// is re-dispatched there with the same messageId.
//
// The module hides the socket, the heartbeat, the correlation of replies and
// the takeover/fallback decision behind Service.Dispatch, the one seam the
// turn worker calls. Nothing above it sees a connection.
//
// Wire (JSON text frames):
//
//	notify -> harness   {"type":"turn","message":<strict A2A message>}
//	harness -> notify   {"kind":"status-update","replyTo":"<messageId>",...}   (ignored for now)
//	                    {"kind":"artifact-update","replyTo":"<messageId>",...} (ignored for now)
//	                    {"kind":"message","role":"agent","replyTo":"<messageId>",
//	                     "contextId":"...","parts":[{"text":"..."}]}           (final reply)
//	                    {"kind":"error","replyTo":"<messageId>","message":"..."} (turn failed)
//
// `replyTo` carries the messageId of the turn being answered. It is a
// top-level field so a harness can tag frames without touching the A2A
// payloads inside them. Several turns may be in flight on one socket.
package a2areceiver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// TokenPrefix marks a token as a receiver key.
const TokenPrefix = "a2arx_"

const (
	secretLen  = 32
	maxNameLen = 64
	touchEvery = time.Minute

	// DefaultTimeout bounds one live turn end to end; it matches the mesh.
	DefaultTimeout = mesh.DefaultTimeout
	// DefaultPingEvery is the heartbeat interval; a ping unanswered within
	// DefaultPingTimeout drops the connection.
	DefaultPingEvery   = 20 * time.Second
	DefaultPingTimeout = 10 * time.Second

	writeTimeout = 10 * time.Second
	readLimit    = 1 << 20

	// closeReplaced is the close reason a connection gets when a newer one
	// for the same agent takes over.
	closeReplaced = "replaced"
)

// ErrInvalidName is returned by Mint for an empty or over-long name.
var ErrInvalidName = fmt.Errorf("a2areceiver: name must be 1-%d characters", maxNameLen)

// errDisconnected means the session a turn was sent on went away.
var errDisconnected = errors.New("live receiver disconnected")

// Service mints receiver keys and routes agent turns to live receivers.
type Service struct {
	store  *store.Store
	agents *agentstore.Store
	log    *slog.Logger

	timeout     time.Duration
	pingEvery   time.Duration
	pingTimeout time.Duration

	mu   sync.Mutex
	live map[string]*session // by agents.id
}

// New builds a Service. log receives audit records (key ids, never tokens);
// nil uses slog.Default.
func New(st *store.Store, agents *agentstore.Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: st, agents: agents, log: log,
		timeout: DefaultTimeout, pingEvery: DefaultPingEvery, pingTimeout: DefaultPingTimeout,
		live: map[string]*session{},
	}
}

// WithTimeout overrides the per-turn deadline on the socket.
func (s *Service) WithTimeout(d time.Duration) *Service {
	s.timeout = d
	return s
}

// WithHeartbeat overrides the ping interval and the time a ping may go
// unanswered.
func (s *Service) WithHeartbeat(every, timeout time.Duration) *Service {
	s.pingEvery, s.pingTimeout = every, timeout
	return s
}

// ---- keys ------------------------------------------------------------------

// Mint creates a receiver key for the agent and returns the stored row plus the
// raw token, the only time the secret exists outside the caller's hands.
// store.ErrNotFound when the agent does not exist in the scope.
func (s *Service) Mint(ctx context.Context, scope domain.Scope, agentIdentifier, name string) (domain.A2AReceiverKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxNameLen {
		return domain.A2AReceiverKey{}, "", ErrInvalidName
	}
	agent, err := s.agents.Agents().GetByIdentifier(ctx, scope, agentIdentifier)
	if err != nil {
		return domain.A2AReceiverKey{}, "", err
	}
	raw := make([]byte, secretLen)
	if _, err := rand.Read(raw); err != nil {
		return domain.A2AReceiverKey{}, "", fmt.Errorf("a2areceiver: mint secret: %w", err)
	}
	key, err := s.store.A2AReceiverKeys().Create(ctx, scope, domain.A2AReceiverKey{
		AgentID:    agent.ID,
		Name:       name,
		SecretHash: hashSecret(raw),
		CreatedAt:  timestamp.Now(),
	})
	if err != nil {
		return domain.A2AReceiverKey{}, "", fmt.Errorf("a2areceiver: create: %w", err)
	}
	s.audit(ctx, "a2a.receiver_key.mint", key, "name", key.Name)
	return key, TokenPrefix + hex.EncodeToString(raw), nil
}

// List returns the agent's keys, revoked included, with the hash cleared.
func (s *Service) List(ctx context.Context, scope domain.Scope, agentIdentifier string) ([]domain.A2AReceiverKey, error) {
	agent, err := s.agents.Agents().GetByIdentifier(ctx, scope, agentIdentifier)
	if err != nil {
		return nil, err
	}
	keys, err := s.store.A2AReceiverKeys().List(ctx, scope, agent.ID)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		keys[i].SecretHash = ""
	}
	return keys, nil
}

// Revoke ends a key immediately and disconnects the live receiver that
// presented it. store.ErrNotFound when the id is absent for the agent or
// already revoked.
func (s *Service) Revoke(ctx context.Context, scope domain.Scope, agentIdentifier, id string) error {
	keys, err := s.List(ctx, scope, agentIdentifier)
	if err != nil {
		return err
	}
	var key *domain.A2AReceiverKey
	for i := range keys {
		if keys[i].ID == id {
			key = &keys[i]
		}
	}
	if key == nil {
		return store.ErrNotFound
	}
	if err := s.store.A2AReceiverKeys().Revoke(ctx, scope, id, timestamp.Now()); err != nil {
		return err
	}
	s.audit(ctx, "a2a.receiver_key.revoke", *key)
	s.mu.Lock()
	sess := s.live[key.AgentID]
	s.mu.Unlock()
	if sess != nil && sess.keyID == id {
		sess.closeAsync("key revoked")
	}
	return nil
}

func (s *Service) audit(ctx context.Context, event string, k domain.A2AReceiverKey, kv ...any) {
	attrs := append([]any{"audit", true, "credential", k.ID, "org", k.OrgID, "env", k.EnvID, "agent", k.AgentID}, kv...)
	s.log.InfoContext(ctx, event, attrs...)
}

func hashSecret(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// authenticate resolves the Authorization header to an active key bound to
// agentID. Every failure is the same error: a wrong agent's key and a revoked
// one are indistinguishable from an unknown one.
func (s *Service) authenticate(ctx context.Context, header, agentID string) (domain.A2AReceiverKey, error) {
	fail := errors.New("unauthorized")
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return domain.A2AReceiverKey{}, fail
	}
	secretHex, ok := strings.CutPrefix(strings.TrimSpace(tok), TokenPrefix)
	if !ok {
		return domain.A2AReceiverKey{}, fail
	}
	raw, err := hex.DecodeString(secretHex)
	if err != nil || len(raw) != secretLen {
		return domain.A2AReceiverKey{}, fail
	}
	key, err := s.store.A2AReceiverKeys().GetBySecretHash(ctx, hashSecret(raw))
	if err != nil || key.RevokedAt != "" || key.AgentID != agentID {
		return domain.A2AReceiverKey{}, fail
	}
	if last, err := timestamp.Parse(key.LastUsedAt); err != nil || time.Since(last) >= touchEvery {
		_ = s.store.A2AReceiverKeys().Touch(ctx, key.ID, timestamp.Now())
	}
	return key, nil
}

// ---- routing ---------------------------------------------------------------

// Live reports whether a receiver is connected for the agent (agents.id) and
// since when.
func (s *Service) Live(agentID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.live[agentID]; sess != nil {
		return sess.since, true
	}
	return time.Time{}, false
}

// Dispatch answers the turn with the agent's live receiver when one is
// connected, otherwise with fallback (the configured runtime). If the live
// receiver goes away mid-turn the turn is dispatched again, with the same
// messageId, to whatever now answers for the agent: a replacement connection,
// else fallback.
func (s *Service) Dispatch(ctx context.Context, t agentdomain.Turn, fallback agentdomain.Runtime) (agentdomain.Reply, error) {
	messageID, msg := mesh.TurnMessage(t)
	t.Message.ID = messageID // the fallback sees the id the receiver saw
	frame, err := json.Marshal(struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}{"turn", msg})
	if err != nil {
		return agentdomain.Reply{}, agentdomain.NewPermanentError(err)
	}

	wctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	for {
		sess := s.session(t.Agent.ID)
		if sess == nil {
			return fallback.Dispatch(ctx, t)
		}
		reply, err := sess.ask(wctx, messageID, frame)
		if errors.Is(err, errDisconnected) {
			s.drop(sess)
			continue
		}
		if err != nil && wctx.Err() != nil && ctx.Err() == nil {
			return agentdomain.Reply{}, fmt.Errorf("no answer from live receiver within %s", s.timeout)
		}
		return reply, err
	}
}

func (s *Service) session(agentID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[agentID]
}

// install makes sess the agent's live receiver, closing any previous one.
func (s *Service) install(sess *session) {
	s.mu.Lock()
	old := s.live[sess.agentID]
	s.live[sess.agentID] = sess
	s.mu.Unlock()
	if old != nil {
		old.closeAsync(closeReplaced)
	}
}

// drop forgets sess (if it is still the live one) and fails its pending turns.
func (s *Service) drop(sess *session) {
	s.mu.Lock()
	if s.live[sess.agentID] == sess {
		delete(s.live, sess.agentID)
	}
	s.mu.Unlock()
	sess.finish()
}

// ---- session ---------------------------------------------------------------

type session struct {
	ws      *websocket.Conn
	agentID string
	keyID   string
	since   time.Time

	mu      sync.Mutex
	pending map[string]chan result
	done    chan struct{}
	once    sync.Once
}

type result struct {
	reply agentdomain.Reply
	err   error
}

func newSession(ws *websocket.Conn, key domain.A2AReceiverKey) *session {
	return &session{
		ws: ws, agentID: key.AgentID, keyID: key.ID, since: time.Now().UTC(),
		pending: map[string]chan result{}, done: make(chan struct{}),
	}
}

// closeAsync sends a close frame with the reason without blocking on the
// peer's close handshake.
func (sess *session) closeAsync(reason string) {
	go func() { _ = sess.ws.Close(websocket.StatusPolicyViolation, reason) }()
}

// finish marks the session gone; asks waiting on it return errDisconnected.
func (sess *session) finish() { sess.once.Do(func() { close(sess.done) }) }

// ask sends one turn frame and waits for the reply correlated to messageID.
func (sess *session) ask(ctx context.Context, messageID string, frame []byte) (agentdomain.Reply, error) {
	ch := make(chan result, 1)
	sess.mu.Lock()
	sess.pending[messageID] = ch
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		if sess.pending[messageID] == ch {
			delete(sess.pending, messageID)
		}
		sess.mu.Unlock()
	}()

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	err := sess.ws.Write(wctx, websocket.MessageText, frame)
	cancel()
	if err != nil {
		return agentdomain.Reply{}, errDisconnected
	}
	select {
	case r := <-ch:
		return r.reply, r.err
	case <-sess.done:
		return agentdomain.Reply{}, errDisconnected
	case <-ctx.Done():
		return agentdomain.Reply{}, ctx.Err()
	}
}

// inbound is a harness frame. Only the fields notify acts on are decoded.
type inbound struct {
	Kind    string             `json:"kind"`
	Role    string             `json:"role"`
	ReplyTo string             `json:"replyTo"`
	Parts   []agentdomain.Part `json:"parts"`
	Message json.RawMessage    `json:"message"`
}

// deliver routes a final frame to the turn it answers. Status and artifact
// updates are accepted and ignored (streaming is a separate feature).
func (sess *session) deliver(f inbound) {
	var r result
	switch {
	case f.Kind == "message" && f.Role == "agent":
		var text strings.Builder
		for _, p := range f.Parts {
			text.WriteString(p.Text)
		}
		answer := strings.TrimSpace(text.String())
		if answer == "" {
			r.err = agentdomain.NewPermanentError(errors.New("live receiver answered with no text"))
		} else {
			r.reply = agentdomain.Reply{Parts: []agentdomain.Part{{Text: answer}}}
		}
	case f.Kind == "error":
		var detail string
		_ = json.Unmarshal(f.Message, &detail)
		r.err = agentdomain.NewPermanentError(fmt.Errorf("live receiver failed the turn: %s", detail))
	default:
		return
	}
	sess.mu.Lock()
	ch := sess.pending[f.ReplyTo]
	sess.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default: // already answered
		}
	}
}

// ---- HTTP ------------------------------------------------------------------

// ServeReceive handles GET /a2a/{agentId}/receive. {agentId} is notify's
// opaque agents.id. The credential is read from the Authorization header only:
// a token in the query string would end up in access logs and proxies, and the
// harnesses this serves are programs, which can set a header.
func (s *Service) ServeReceive(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	key, err := s.authenticate(r.Context(), r.Header.Get("Authorization"), agentID)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has answered
	}
	ws.SetReadLimit(readLimit)
	sess := newSession(ws, key)
	s.install(sess)
	s.audit(r.Context(), "a2a.receiver.connect", key)
	defer func() {
		s.drop(sess)
		_ = ws.CloseNow()
		s.audit(context.Background(), "a2a.receiver.disconnect", key)
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go s.heartbeat(ctx, sess)

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var f inbound
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		sess.deliver(f)
	}
}

// heartbeat pings until the session ends; an unanswered ping drops the socket,
// which ends the read loop and fails the session's turns over.
func (s *Service) heartbeat(ctx context.Context, sess *session) {
	tick := time.NewTicker(s.pingEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.done:
			return
		case <-tick.C:
			pctx, cancel := context.WithTimeout(ctx, s.pingTimeout)
			err := sess.ws.Ping(pctx)
			cancel()
			if err != nil {
				_ = sess.ws.CloseNow()
				return
			}
		}
	}
}
