// Package a2a serves notify's A2A channel: a JSON-RPC 2.0 endpoint
// (A2A v0.3) and the agent card, both addressed by notify's opaque agent id,
// at /a2a/{agentId}. Callers authenticate with an A2A client key or a
// subscriber session (package a2aauth); this package never sees a tenant API
// key.
//
// A task is the user activity a message/send wrote: its id is the activity id
// (also the mesh messageId), and its state is read off the conversation --
// completed once the turn's reply activity exists, failed once the turn's
// engine run ended unsuccessfully, working otherwise. A small status record
// (package tasks) adds the progress notes the agent reported and any final
// state it chose (input-required, canceled). message/stream and
// tasks/resubscribe deliver the task's changes as server-sent events, and
// GET /a2a/{agentId}/ws carries the same events over a WebSocket.
// A message/send goes through ingress.Compose, the path every other inbound
// channel uses, so the conversation shows in notify's transcript like any
// other.
//
// Isolation: an agent id resolves inside the credential's own org and
// environment, so a key from another environment gets the same 404 as an
// unknown id. A context id is namespaced by the credential
// (platform_thread_id = "a2a:<credential digest>:<contextId>"), so two callers
// can neither collide on a context id nor reach each other's conversations.
package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/a2aauth"
	"github.com/danmestas/notify/internal/agents/cards"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

const (
	// maxBodyBytes caps a request body. The inbox compose route sets no cap of
	// its own; this one leaves room for the largest attachment set
	// (256 KiB of text, base64-encoded) plus the envelope.
	maxBodyBytes = 1 << 20

	// defaultWait is how long message/send waits for the reply before
	// answering with a working task.
	defaultWait = 5 * time.Second

	pollEvery     = 100 * time.Millisecond
	runCheckEvery = time.Second

	// Per-minute message/send budgets, per credential and per agent.
	credentialLimit = 60
	agentLimit      = 300
	limitWindow     = time.Minute
)

// Composer starts a turn. *ingress.Service satisfies it.
type Composer interface {
	Compose(ctx context.Context, scope domain.Scope, in ingress.ComposeInput) (ingress.ComposeResult, error)
}

// Runs reads a turn's engine run. *engine.Engine satisfies it.
type Runs interface {
	GetRun(ctx context.Context, runID string) (engine.RunSnapshot, error)
}

// Tasks is the turn status record (package tasks): its stored state and notes,
// and a signal when they change in this process. Optional; without it tasks
// carry no notes and streams follow the activity log by polling.
type Tasks interface {
	Get(ctx context.Context, scope domain.Scope, taskID string) (agentdomain.TaskStatus, bool)
	Watch(scope domain.Scope, taskID string) (<-chan struct{}, func())
}

// Limiter is the shared per-key budget (eventbus.Bus.BudgetAllow).
type Limiter interface {
	BudgetAllow(key string, cost, limit int64, window time.Duration) (bool, error)
}

// Service is the A2A channel.
type Service struct {
	agents  *agentstore.Store
	compose Composer
	runs    Runs
	cards   cards.Lookup
	limiter Limiter
	tasks   Tasks
	base    string
	wait    time.Duration
	log     *slog.Logger
}

// New builds the service. runs may be nil, in which case a turn is never
// reported failed.
func New(agents *agentstore.Store, compose Composer, runs Runs) *Service {
	return &Service{agents: agents, compose: compose, runs: runs, wait: defaultWait, log: slog.Default()}
}

// WithCards serves mesh agents' published cards as the base of their A2A card.
func (s *Service) WithCards(c cards.Lookup) *Service { s.cards = c; return s }

// WithTasks reads task status notes from t and wakes streams when they change.
func (s *Service) WithTasks(t Tasks) *Service { s.tasks = t; return s }

// WithLimiter rate-limits message/send per credential and per agent.
func (s *Service) WithLimiter(l Limiter) *Service { s.limiter = l; return s }

// WithPublicBaseURL sets the origin card URLs are built on. Unset, the
// request's own scheme and host are used.
func (s *Service) WithPublicBaseURL(base string) *Service {
	s.base = strings.TrimRight(base, "/")
	return s
}

// WithWait sets how long message/send waits for a reply before returning a
// working task.
func (s *Service) WithWait(d time.Duration) *Service { s.wait = d; return s }

// WithLogger sets the logger.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	if l != nil {
		s.log = l
	}
	return s
}

// Routes mounts the A2A routes on r. The caller puts a2aauth.Authenticate in
// front of them.
func (s *Service) Routes(r chi.Router) {
	r.Post("/a2a/{agentId}", s.rpc)
	r.Get("/a2a/{agentId}/.well-known/agent-card.json", s.card)
	r.Get("/a2a/{agentId}/ws", s.serveWS)
}

// caller resolves the principal and the agent it may address. ok is false once
// the 404 has been written: an unknown agent and one in another environment
// look the same.
func (s *Service) caller(w http.ResponseWriter, r *http.Request) (a2aauth.Principal, agentdomain.Agent, bool) {
	p, ok := a2aauth.FromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return p, agentdomain.Agent{}, false
	}
	agent, err := s.agents.Agents().GetByID(r.Context(), p.Scope, chi.URLParam(r, "agentId"))
	if errors.Is(err, agentstore.ErrNotFound) {
		http.NotFound(w, r)
		return p, agent, false
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "a2a: load agent", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return p, agent, false
	}
	return p, agent, true
}

// threadPrefix namespaces a credential's contexts. The digest keeps credential
// ids (which may contain ':') from ever being a prefix of one another.
func threadPrefix(credentialID string) string {
	sum := sha256.Sum256([]byte(credentialID))
	return "a2a:" + hex.EncodeToString(sum[:8]) + ":"
}

// card serves the agent card.
func (s *Service) card(w http.ResponseWriter, r *http.Request) {
	p, agent, ok := s.caller(w, r)
	if !ok || !s.allow(w, r, p, agent) {
		return
	}
	raw, err := s.buildCard(agent, s.baseURL(r))
	if err != nil {
		s.log.ErrorContext(r.Context(), "a2a: build card", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *Service) baseURL(r *http.Request) string {
	if s.base != "" {
		return s.base
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// buildCard starts from the receiver's card (a mesh agent's published card, or
// one derived for a managed agent), falls back to a minimal one built from the
// record, and then sets the fields notify owns: where it is served, how it is
// called, whether it streams and how it authenticates. The receiver's name,
// description, skills and extensions are kept.
func (s *Service) buildCard(a agentdomain.Agent, base string) ([]byte, error) {
	card := map[string]json.RawMessage{}
	if raw := s.sourceCard(a); raw != nil {
		if err := json.Unmarshal(raw, &card); err != nil || card == nil {
			card = map[string]json.RawMessage{}
		}
	}
	setDefault := func(k string, v any) {
		if _, ok := card[k]; !ok {
			card[k] = mustJSON(v)
		}
	}
	setDefault("name", a.Name)
	setDefault("description", a.Description)
	setDefault("version", a.UpdatedAt)
	setDefault("protocolVersion", "0.3.0")
	setDefault("defaultInputModes", []string{"text/plain"})
	setDefault("defaultOutputModes", []string{"text/plain"})
	setDefault("skills", []struct{}{})

	// notify is the only endpoint: drop what would advertise another
	// transport or an extended card it doesn't serve, and signatures the
	// rewrite below would invalidate.
	for _, k := range []string{"additionalInterfaces", "supportsAuthenticatedExtendedCard", "signatures"} {
		delete(card, k)
	}
	card["url"] = mustJSON(base + "/a2a/" + a.ID)
	card["preferredTransport"] = mustJSON("JSONRPC")
	card["securitySchemes"] = mustJSON(map[string]any{"bearer": map[string]string{"type": "http", "scheme": "bearer"}})
	card["security"] = mustJSON([]map[string][]string{{"bearer": {}}})

	caps := map[string]json.RawMessage{}
	if raw, ok := card["capabilities"]; ok {
		_ = json.Unmarshal(raw, &caps)
		if caps == nil {
			caps = map[string]json.RawMessage{}
		}
	}
	caps["streaming"] = mustJSON(true)
	card["capabilities"] = mustJSON(caps)
	return json.Marshal(card)
}

func (s *Service) sourceCard(a agentdomain.Agent) json.RawMessage {
	switch a.Runtime {
	case agentdomain.RuntimeMesh:
		if key := cards.Key(a.MeshSubject); key != "" && s.cards != nil {
			if c, ok := s.cards.Card(key); ok {
				return c.Raw
			}
		}
	case agentdomain.RuntimeManaged:
		return cards.Derive(a)
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only called on values built in this file
	}
	return b
}
