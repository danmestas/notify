// Package ingress owns the agent inbound path (RFC 0002 §3): the inbox
// compose endpoint that turns an authenticated message into a durable
// conversation activity plus a dagnats turn run, the turn worker that
// dispatches the run through the agent's runtime, and the reply path that
// lands the answer back on the inbox — an agent activity, an in_app messages
// row, and the existing realtime fan-out. It also serves the transcript read.
package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/identity"
	corestore "github.com/danmestas/notify/internal/store"
)

// TaskMesh is the engine task type for mesh-runtime turns (RFC 0002 §5,
// Task("agent.turn.<runtime>")).
const TaskMesh = "agent.turn.mesh"

// historyWindow bounds the transcript carried into each turn,
// RFC 0002 §4: last N=20, oldest first).
const historyWindow = 20

// EventPublisher publishes the per-subscriber live inbox event for a
// persisted reply message. eventbus.Bus satisfies this structurally (ingress
// does not import eventbus). A nil publisher disables live publishing.
type EventPublisher interface {
	PublishInbox(environmentID, subscriberID string, data []byte) error
}

// ReplyHook is an optional post-persist callback invoked after an agent reply
// lands on a conversation. The email two-way loop uses it to mail the reply
// back out on email-origin conversations (emailloop.Service.ReplyHook
// satisfies it). It runs best-effort: a hook is never allowed to fail the turn,
// since the reply is already durably persisted. conv is the conversation the
// reply landed on; text is the concatenated reply body.
type ReplyHook func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string)

// Service wires the compose endpoint, the transcript read, and the turn
// worker around the agents store, the core store, and the engine.
type Service struct {
	agents    *agentstore.Store
	core      *corestore.Store
	eng       *engine.Engine
	bus       EventPublisher
	replyHook ReplyHook
	retry     *engine.RetryPolicy
	runtimes  map[string]agentdomain.Runtime

	mu         sync.Mutex
	registered map[string]bool
}

// New constructs a Service. Register runtimes with WithRuntime before serving.
func New(agents *agentstore.Store, core *corestore.Store, eng *engine.Engine, bus EventPublisher) *Service {
	return &Service{
		agents: agents,
		core:   core,
		eng:    eng,
		bus:    bus,
		// MaxAttempts=3: the engine retries transient turn failures with
		// backoff before dead-lettering, giving mesh agents up to 4 total
		// dispatch attempts (dagnats retry semantics: ≤ MaxAttempts
		// retries after the first attempt). The fix landed in
		// danmestas/dagnats#384: the bridge pollResponse now always carries
		// Attempt≥1, so the retry counter advances correctly and the
		// dedup-by-Nats-Msg-Id stall that made MaxAttempts>0 unsafe is
		// gone.
		retry: &engine.RetryPolicy{
			MaxAttempts:  3,
			Strategy:     "fixed",
			InitialDelay: 2 * time.Second,
			MaxDelay:     2 * time.Second,
		},
		runtimes:   map[string]agentdomain.Runtime{},
		registered: map[string]bool{},
	}
}

// WithRuntime registers the executor for a runtime name (e.g. "mesh") and
// returns the service for chaining. Compose rejects agents whose runtime has
// no registered executor, so runs are never stranded on an unpolled task.
func (s *Service) WithRuntime(name string, rt agentdomain.Runtime) *Service {
	s.runtimes[name] = rt
	return s
}

// WithRetry overrides the turn step's retry policy and returns the service
// for chaining (tests use a fast policy to reach the DLQ quickly).
func (s *Service) WithRetry(p *engine.RetryPolicy) *Service {
	s.retry = p
	return s
}

// WithReplyHook attaches an optional post-persist reply callback and returns
// the service for chaining. The email two-way loop sets it so an agent reply on
// an email-origin conversation is mailed back out. A nil hook (the default)
// disables the callback.
func (s *Service) WithReplyHook(h ReplyHook) *Service {
	s.replyHook = h
	return s
}

// Routes mounts the inbox conversation endpoints. They run behind the same
// ApiKey guard as every other /v1 route; the subscriber-JWT handshake of
// RFC 0002 §8 does not exist yet.
// Tracking: switch compose auth to the subscriber JWT once realtime grows it.
func (s *Service) Routes(r chi.Router) {
	r.Post("/v1/inbox/conversations/{agentIdentifier}/messages", s.compose)
	r.Get("/v1/inbox/conversations/{agentIdentifier}", s.transcript)
}

// composeRequest is the POST /v1/inbox/conversations/{agentIdentifier}/messages
// body. ConversationID is the inbox thread handle (the conversation's
// platform_thread_id); empty starts a new thread.
type composeRequest struct {
	SubscriberID   string `json:"subscriberId"`
	ConversationID string `json:"conversationId"`
	Text           string `json:"text"`
}

// composeResponse acknowledges an accepted turn: the thread handle to keep
// composing into, the appended user activity, and the engine run executing
// the turn.
type composeResponse struct {
	ConversationID string `json:"conversationId"`
	ActivityID     string `json:"activityId"`
	RunID          string `json:"runId"`
}

// turnInput is the engine run input. Field names are wire contract: the
// workflow's idempotency key dot-path reads "activity_id" and the singleton
// key dot-path reads "conversation_id".
type turnInput struct {
	OrgID           string `json:"org_id"`
	EnvID           string `json:"env_id"`
	AgentIdentifier string `json:"agent_identifier"`
	ConversationID  string `json:"conversation_id"`
	ActivityID      string `json:"activity_id"`
}

// compose resolves the agent, upserts the conversation, appends the user
// activity, and starts the turn run. It answers 202: the turn executes as
// engine work, never in-request (RFC 0002 §3).
func (s *Service) compose(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	var req composeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		http.Error(w, "text is required", http.StatusUnprocessableEntity)
		return
	}
	if req.SubscriberID == "" {
		http.Error(w, "subscriberId is required", http.StatusUnprocessableEntity)
		return
	}

	res, err := s.ComposeMessage(r.Context(), scope, chi.URLParam(r, "agentIdentifier"),
		req.ConversationID, req.SubscriberID, req.Text)
	if err != nil {
		writeComposeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(composeResponse(res))
}

// ComposeResult is the outcome of a composed turn: the conversation thread
// handle to keep composing into, the appended user activity, and the engine
// run executing the turn.
type ComposeResult struct {
	ConversationID string // platform_thread_id of the conversation
	ActivityID     string
	RunID          string
}

// ErrUnknownRuntime marks an agent whose runtime has no registered executor.
// Callers map it to 422 (the turn cannot be dispatched on this server).
var ErrUnknownRuntime = errors.New("agent runtime is not executable on this server")

// ComposeMessage resolves the agent, upserts the conversation, appends the
// user activity, and starts the turn run. It is the in-process entry point the
// HTTP compose handler and the console conversation view both call — the turn
// always executes as engine work, never in-request (RFC 0002 §3). An empty
// conversationID starts a new thread.
func (s *Service) ComposeMessage(
	ctx context.Context, scope domain.Scope,
	agentIdentifier, conversationID, subscriberID, text string,
) (ComposeResult, error) {
	if text == "" {
		return ComposeResult{}, fmt.Errorf("text is required")
	}
	if subscriberID == "" {
		return ComposeResult{}, fmt.Errorf("subscriberId is required")
	}

	agent, err := s.agents.Agents().GetByIdentifier(ctx, scope, agentIdentifier)
	if err != nil {
		return ComposeResult{}, err
	}
	if _, ok := s.runtimes[agent.Runtime]; !ok {
		return ComposeResult{}, fmt.Errorf("%w: %q", ErrUnknownRuntime, agent.Runtime)
	}

	// Inbox adapter contract (RFC 0002 §6): integration_id is empty (no
	// channel integration) and platform_thread_id is the inbox conversation
	// id — client-supplied for thread continuity, minted for a new thread.
	threadID := conversationID
	if threadID == "" {
		threadID = corestore.NewULID()
	}
	conv, err := s.agents.Conversations().Upsert(ctx, scope, agentdomain.Conversation{
		AgentID:          agent.ID,
		IntegrationID:    "",
		PlatformThreadID: threadID,
		SubscriberID:     subscriberID,
	})
	if err != nil {
		return ComposeResult{}, err
	}

	content, err := json.Marshal(map[string][]agentdomain.Part{"parts": {{Text: text}}})
	if err != nil {
		return ComposeResult{}, err
	}
	act, err := s.agents.Activities().Append(ctx, scope, agentdomain.Activity{
		ConversationID: conv.ID,
		Role:           "user",
		Content:        content,
	})
	if err != nil {
		return ComposeResult{}, err
	}

	runID, err := s.startTurn(ctx, scope, agent, conv.ID, act.ID)
	if err != nil {
		return ComposeResult{}, err
	}

	return ComposeResult{
		ConversationID: conv.PlatformThreadID,
		ActivityID:     act.ID,
		RunID:          runID,
	}, nil
}

// writeComposeError maps a ComposeMessage error to its HTTP status. Store
// not-found stays a 404; an unknown runtime is a 422; everything else is a 500.
func writeComposeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentstore.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrUnknownRuntime):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// startTurn ensures the agent's turn workflow exists and starts one run for
// the activity. Idempotency keys on the activity id (webhook-redelivery
// safe); the singleton key serialises turns per conversation so turns within
// a thread never interleave (the digest keying pattern, RFC 0002 §5).
func (s *Service) startTurn(ctx context.Context, scope domain.Scope, agent agentdomain.Agent, conversationID, activityID string) (string, error) {
	name, err := s.ensureWorkflow(ctx, agent)
	if err != nil {
		return "", err
	}
	input, err := json.Marshal(turnInput{
		OrgID:           scope.OrgID,
		EnvID:           scope.EnvID,
		AgentIdentifier: agent.Identifier,
		ConversationID:  conversationID,
		ActivityID:      activityID,
	})
	if err != nil {
		return "", err
	}
	return s.eng.StartRun(ctx, name, input)
}

// ensureWorkflow registers the per-agent turn workflow once per process.
// The name is "agent-turn-<agentID>" — dagnats stores definitions in a NATS
// KV bucket whose key charset excludes ':', so the RFC's "agent-turn:<id>"
// spelling is flattened to a hyphen.
func (s *Service) ensureWorkflow(ctx context.Context, agent agentdomain.Agent) (string, error) {
	name := "agent-turn-" + agent.ID
	s.mu.Lock()
	done := s.registered[name]
	s.mu.Unlock()
	if done {
		return name, nil
	}
	def := engine.WorkflowDef{
		Name:           name,
		IdempotencyKey: "activity_id",
		Singleton:      &engine.SingletonConfig{Mode: "skip", Key: "conversation_id"},
		Steps: []engine.StepDef{{
			ID:    "turn",
			Task:  "agent.turn." + agent.Runtime,
			Type:  "normal",
			Retry: s.retry,
		}},
	}
	if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.registered[name] = true
	s.mu.Unlock()
	return name, nil
}

// transcriptResponse is the GET /v1/inbox/conversations/{agentIdentifier}
// body: the conversation row plus its full activity log oldest-first.
type transcriptResponse struct {
	Conversation agentdomain.Conversation `json:"conversation"`
	Activities   []agentdomain.Activity   `json:"activities"`
}

// transcript serves the conversation read (RFC 0002 §7):
// GET /v1/inbox/conversations/{agentIdentifier}?conversationId=<thread id>
//
// Optional pagination parameters:
//
//	limit  — max rows to return (default 50, max 200)
//	before — ULID; returns up to limit rows older than this id
//	after  — ULID; returns up to limit rows newer than this id
//
// When neither before nor after is supplied the most recent limit rows are
// returned (useful for initial render and SSE catch-up). When before is
// supplied it takes precedence over after.
func (s *Service) transcript(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	q := r.URL.Query()
	threadID := q.Get("conversationId")
	if threadID == "" {
		http.Error(w, "conversationId is required", http.StatusBadRequest)
		return
	}
	agent, err := s.agents.Agents().GetByIdentifier(r.Context(), scope, chi.URLParam(r, "agentIdentifier"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	conv, err := s.agents.Conversations().GetByThread(r.Context(), scope, agent.ID, "", threadID)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	limit := parseActivityLimit(q.Get("limit"))
	before := q.Get("before")
	after := q.Get("after")

	var acts []agentdomain.Activity
	if before != "" || after != "" || q.Get("limit") != "" {
		// Cursor / paginated path.
		acts, err = s.agents.Activities().Page(r.Context(), scope, conv.ID, limit, before, after)
	} else {
		// Legacy full-transcript path: return everything oldest-first.
		acts, err = s.agents.Activities().History(r.Context(), scope, conv.ID, 0)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if acts == nil {
		acts = []agentdomain.Activity{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(transcriptResponse{Conversation: conv, Activities: acts})
}

// parseActivityLimit converts the raw limit query param to an int. Zero or
// empty → 0 (the store layer applies its own default and cap).
func parseActivityLimit(s string) int {
	if s == "" {
		return 0
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int(c-'0')
	}
	return v
}

// HandleTurn is the agent.turn.mesh task handler, registered via
// engine.Handle(ingress.TaskMesh, svc.HandleTurn). It rebuilds the Turn from
// the store, dispatches it through the agent's runtime, and on success
// appends the agent activity, persists the in_app messages row, and publishes
// the realtime inbox event. Transient dispatch failures return retryable
// errors (engine retries, then DLQ — replay resumes the conversation);
// permanent ones go straight to the DLQ.
func (s *Service) HandleTurn(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var in turnInput
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return nil, engine.NewNonRetryableError(fmt.Errorf("decode turn input: %w", err))
	}
	scope := domain.Scope{OrgID: in.OrgID, EnvID: in.EnvID}

	agent, err := s.agents.Agents().GetByIdentifier(ctx, scope, in.AgentIdentifier)
	if err != nil {
		return nil, classifyStoreError("load agent", err)
	}
	conv, err := s.agents.Conversations().Get(ctx, scope, in.ConversationID)
	if err != nil {
		return nil, classifyStoreError("load conversation", err)
	}
	history, msg, err := s.loadTurnContext(ctx, scope, conv.ID, in.ActivityID)
	if err != nil {
		return nil, err
	}

	rt, ok := s.runtimes[agent.Runtime]
	if !ok {
		return nil, engine.NewNonRetryableError(fmt.Errorf("no executor for runtime %q", agent.Runtime))
	}
	reply, err := rt.Dispatch(ctx, agentdomain.Turn{
		Agent:        agent,
		Conversation: conv,
		History:      history,
		Message:      msg,
	})
	if err != nil {
		if agentdomain.IsPermanent(err) {
			return nil, engine.NewNonRetryableError(err)
		}
		return nil, engine.NewRetryableError(err)
	}

	out, err := s.deliverReply(ctx, scope, conv, t.RunID, reply)
	if err != nil {
		// The reply exists but persisting it failed; retrying re-runs the
		// whole turn, which beats silently dropping the answer.
		return nil, engine.NewRetryableError(err)
	}
	return out, nil
}

// loadTurnContext fetches the bounded history window and splits out the
// inbound user activity. A replayed turn whose activity has scrolled past the
// window falls back to the full transcript so the message is never lost.
// Both paths deliver exactly historyWindow entries (when that many exist)
// with the inbound message excluded: the normal path fetches one extra row
// to cover the exclusion, and both trim identically after the split.
func (s *Service) loadTurnContext(ctx context.Context, scope domain.Scope, conversationID, activityID string) ([]agentdomain.Activity, agentdomain.Activity, error) {
	acts, err := s.agents.Activities().History(ctx, scope, conversationID, historyWindow+1)
	if err != nil {
		return nil, agentdomain.Activity{}, engine.NewRetryableError(fmt.Errorf("load history: %w", err))
	}
	history, msg, found := splitMessage(acts, activityID)
	if !found {
		full, err := s.agents.Activities().History(ctx, scope, conversationID, 0)
		if err != nil {
			return nil, agentdomain.Activity{}, engine.NewRetryableError(fmt.Errorf("load full history: %w", err))
		}
		if history, msg, found = splitMessage(full, activityID); !found {
			return nil, agentdomain.Activity{}, engine.NewNonRetryableError(
				fmt.Errorf("activity %s not found in conversation %s", activityID, conversationID),
			)
		}
	}
	if len(history) > historyWindow {
		history = history[len(history)-historyWindow:]
	}
	return history, msg, nil
}

// splitMessage removes the inbound activity from the window, returning the
// remaining history (still oldest-first) and the message itself.
func splitMessage(acts []agentdomain.Activity, activityID string) ([]agentdomain.Activity, agentdomain.Activity, bool) {
	for i, a := range acts {
		if a.ID == activityID {
			history := make([]agentdomain.Activity, 0, len(acts)-1)
			history = append(history, acts[:i]...)
			history = append(history, acts[i+1:]...)
			return history, a, true
		}
	}
	return acts, agentdomain.Activity{}, false
}

// deliverReply appends the agent activity, persists the in_app messages row,
// and publishes the realtime inbox event (the same write+publish path the
// send.in_app handler uses). Returns the persisted message JSON as the task
// output.
func (s *Service) deliverReply(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, runID string, reply agentdomain.Reply) (json.RawMessage, error) {
	content, err := json.Marshal(map[string]any{"parts": reply.Parts})
	if err != nil {
		return nil, fmt.Errorf("marshal reply content: %w", err)
	}
	if _, err := s.agents.Activities().Append(ctx, scope, agentdomain.Activity{
		ConversationID: conv.ID,
		Role:           "agent",
		Content:        content,
		RunID:          runID,
	}); err != nil {
		return nil, fmt.Errorf("append agent activity: %w", err)
	}

	var text string
	for _, p := range reply.Parts {
		text += p.Text
	}
	saved, err := s.core.Messages().Create(ctx, scope, domain.Message{
		ID:           corestore.NewULID(),
		OrgID:        scope.OrgID,
		EnvID:        scope.EnvID,
		RunID:        runID,
		SubscriberID: conv.SubscriberID,
		Channel:      "in_app",
		Content:      text,
		Status:       "sent",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("persist reply message: %w", err)
	}
	out, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	// Best-effort live publish: the message is already persisted, so a
	// publish failure must not fail (and re-run) the turn.
	if s.bus != nil && conv.SubscriberID != "" {
		_ = s.bus.PublishInbox(scope.EnvID, conv.SubscriberID, out)
	}
	// Best-effort close-the-loop: on an email-origin conversation the hook
	// mails the reply back out with the same tokenized Reply-To. Like the
	// publish above it must never fail (and re-run) the turn — the reply is
	// already persisted.
	if s.replyHook != nil {
		s.replyHook(ctx, scope, conv, text)
	}
	return out, nil
}

// classifyStoreError maps a not-found row to a permanent failure (replaying
// cannot conjure the row) and anything else to a transient one.
func classifyStoreError(op string, err error) error {
	if errors.Is(err, agentstore.ErrNotFound) {
		return engine.NewNonRetryableError(fmt.Errorf("%s: %w", op, err))
	}
	return engine.NewRetryableError(fmt.Errorf("%s: %w", op, err))
}

// writeStoreError renders an agents-store error: ErrNotFound as 404,
// anything else as 500.
func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, agentstore.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
