package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// Agent-loop wiring constants. The builder runs as a single agent_loop step
// whose task is driven iteratively by the dagnats engine; one model turn per
// iteration. DefaultMaxIterations mirrors the synchronous Builder's maxRounds —
// the engine MaxIterations bound is the durable equivalent of "exceeded max
// rounds" and the hard cap on a runaway self-correct.
const (
	AgentLoopTask        = "ai.build"
	AgentLoopWorkflow    = "ai-builder"
	AgentLoopStepID      = "build"
	DefaultMaxIterations = 24
)

// maxSelfCorrectAttempts caps consecutive turns that fail validation — either a
// per-tool-call steptype.Validate rejection (draft.go's applyToolCall) or a
// turn-end validateDraft/Compile failure — before the loop gives up. Without
// this cap a persistently wrong model would ride the self-correct message all
// the way to DefaultMaxIterations, one iteration at a time; failing fast
// surfaces the problem sooner and far cheaper. A turn whose tool calls all
// succeed (or a validateDraft pass) resets the counter, so it only trips on
// genuinely consecutive failures.
const maxSelfCorrectAttempts = 3

// defaultTokenBudget is the fallback per-run input+output token ceiling used
// when NOTIFY_AI_TOKEN_BUDGET is unset. It is comfortably above what a normal
// build (a couple dozen tool-call turns against a several-KB system prompt)
// consumes, while still bounding a runaway/looping model's spend; 0 disables
// the budget entirely.
const defaultTokenBudget = 300_000

// tokenBudgetEnvVar names the environment variable that overrides
// defaultTokenBudget without requiring a main.go / wiring change: unset or
// unparsable falls back to defaultTokenBudget, "0" disables the budget.
const tokenBudgetEnvVar = "NOTIFY_AI_TOKEN_BUDGET"

// SessionStore is the slice of store.BuilderSessionRepo the agent-loop handler
// needs: load the originating session (prompt + scope) and persist the final
// draft. store.Store.BuilderSessions() satisfies it directly.
type SessionStore interface {
	Get(ctx context.Context, scope domain.Scope, id string) (store.BuilderSession, error)
	UpdateDraft(ctx context.Context, scope domain.Scope, id string, draft json.RawMessage, status string) error
	// Messages returns the session's message history in insertion order. The
	// generalized loadState reads it to drive a follow-up turn from the latest
	// user instruction (a copilot chat message) rather than the original prompt.
	Messages(ctx context.Context, scope domain.Scope, sessionID string) ([]store.BuilderMessage, error)
}

// loopContext is the subset of *engine.AgentLoopContext the per-iteration
// handler drives. Modeling it as an interface lets handle() be unit-tested with
// a fake that records terminal actions and feeds the Continue checkpoint back as
// the next iteration's Checkpoint() — no live engine or NATS required.
// *engine.AgentLoopContext satisfies it structurally.
type loopContext interface {
	Iteration() int
	Checkpoint() []byte
	Input() json.RawMessage
	Continue(checkpoint []byte) error
	Complete(output json.RawMessage) error
	Stream(data []byte) error
}

// loopState is the full agent-loop checkpoint: it round-trips verbatim through
// the engine's checkpoint KV between iterations, so each dispatch reconstructs
// the conversation and the in-progress draft without any other persisted state.
// Scope, SessionID and GatedTypes are frozen on iteration 0 (the tool enum stays
// stable across the run, matching the synchronous Builder which gates once).
type loopState struct {
	SessionID  string        `json:"sessionId"`
	Scope      domain.Scope  `json:"scope"`
	GatedTypes []string      `json:"gatedTypes"`
	Messages   []Message     `json:"messages"`
	Draft      WorkflowDraft `json:"draft"`
	// Mode is store.BuilderSessionModeCreate (build from empty) or
	// store.BuilderSessionModeRefine (edit an existing workflow). Frozen on
	// iteration 0 and carried in the checkpoint so every iteration picks the same
	// system prompt.
	Mode string `json:"mode,omitempty"`
	// Original snapshots the refined workflow's starting shape so the refine
	// system prompt keeps describing the pre-edit workflow (name + ordered steps
	// with their stepIds) even as Draft mutates across iterations. Empty in
	// create mode.
	Original *WorkflowDraft `json:"original,omitempty"`
	// SelfCorrectAttempts counts consecutive iterations that failed validation
	// (a per-tool-call Validate rejection or a turn-end validateDraft failure).
	// It resets to 0 on any iteration that fully succeeds, and trips
	// maxSelfCorrectAttempts into a terminal KindError rather than let a
	// persistently wrong model burn the whole MaxIterations budget.
	SelfCorrectAttempts int `json:"selfCorrectAttempts,omitempty"`
	// TokensUsed accumulates InputTokens+OutputTokens from every successful
	// model call this run has made so far. It round-trips through the
	// checkpoint like everything else in loopState (no separate store or engine
	// plumbing) and is compared against AgentLoopBuilder.tokenBudget each
	// iteration.
	TokensUsed int64 `json:"tokensUsed,omitempty"`
}

// runInput is the StartRun payload: the session to build into plus its tenancy
// scope (needed to load the scope-filtered session on iteration 0).
type runInput struct {
	SessionID string `json:"sessionId"`
	OrgID     string `json:"orgId"`
	EnvID     string `json:"envId"`
}

// streamEvent is the JSON-on-the-wire form of a builder Event published to
// stream.{runID}.{stepID}. It mirrors Event's shape but renders Err as a string
// (an error value does not marshal). The console (next unit) decodes these.
type streamEvent struct {
	Kind       EventKind      `json:"kind"`
	Text       string         `json:"text,omitempty"`
	ToolName   string         `json:"toolName,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Draft      *WorkflowDraft `json:"draft,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// AgentLoopBuilder hosts the AI workflow builder as a durable dagnats agent-loop
// handler. It owns the same reusable pieces as the synchronous Builder (Client,
// registry, integration gating) plus the engine and session store needed to
// trigger runs and persist results. It is added alongside Builder.Run, not a
// replacement.
type AgentLoopBuilder struct {
	eng         *engine.Engine
	client      Client
	reg         *steptype.Registry
	ints        IntegrationLister
	sessions    SessionStore
	model       string
	maxSteps    int
	maxIters    int
	tokenBudget int
}

// AgentLoopOption configures an AgentLoopBuilder.
type AgentLoopOption func(*AgentLoopBuilder)

// WithAgentLoopModel overrides the default Claude model.
func WithAgentLoopModel(model string) AgentLoopOption {
	return func(b *AgentLoopBuilder) {
		if model != "" {
			b.model = model
		}
	}
}

// WithAgentLoopMaxIterations caps the engine-side iteration bound declared on
// the workflow's agent_loop step. Non-positive values are ignored.
func WithAgentLoopMaxIterations(n int) AgentLoopOption {
	return func(b *AgentLoopBuilder) {
		if n > 0 {
			b.maxIters = n
		}
	}
}

// WithAgentLoopMaxSteps caps how many steps a single draft may hold. Non-positive
// values are ignored.
func WithAgentLoopMaxSteps(n int) AgentLoopOption {
	return func(b *AgentLoopBuilder) {
		if n > 0 {
			b.maxSteps = n
		}
	}
}

// WithAgentLoopRegistry overrides the step-type registry (defaults to
// steptype.Default()).
func WithAgentLoopRegistry(reg *steptype.Registry) AgentLoopOption {
	return func(b *AgentLoopBuilder) {
		if reg != nil {
			b.reg = reg
		}
	}
}

// WithAgentLoopTokenBudget caps the total InputTokens+OutputTokens a single run
// may spend across all its iterations (see loopState.TokensUsed). Negative
// values are ignored (NewAgentLoopBuilder's env-derived default already applies
// 0 = disabled, so this option only needs to guard against accidental negative
// overrides).
func WithAgentLoopTokenBudget(n int) AgentLoopOption {
	return func(b *AgentLoopBuilder) {
		if n >= 0 {
			b.tokenBudget = n
		}
	}
}

// NewAgentLoopBuilder constructs an AgentLoopBuilder over the engine, a Client,
// the integration lister (provider gating) and the builder-session store. The
// token budget defaults from NOTIFY_AI_TOKEN_BUDGET (falling back to
// defaultTokenBudget when unset/unparsable; "0" disables it) so callers such as
// cmd/notify need no wiring change to get a sane default — WithAgentLoopTokenBudget
// still overrides it explicitly for tests or a future flag.
func NewAgentLoopBuilder(eng *engine.Engine, client Client, ints IntegrationLister, sessions SessionStore, opts ...AgentLoopOption) *AgentLoopBuilder {
	b := &AgentLoopBuilder{
		eng:         eng,
		client:      client,
		reg:         steptype.Default(),
		ints:        ints,
		sessions:    sessions,
		model:       DefaultModel,
		maxSteps:    DefaultMaxSteps,
		maxIters:    DefaultMaxIterations,
		tokenBudget: tokenBudgetFromEnv(os.Getenv),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// tokenBudgetFromEnv reads tokenBudgetEnvVar via getenv and returns its parsed
// non-negative value, or defaultTokenBudget when the variable is unset,
// unparsable, or negative. getenv is injected so tests can exercise this
// without mutating the process environment.
func tokenBudgetFromEnv(getenv func(string) string) int {
	raw := getenv(tokenBudgetEnvVar)
	if raw == "" {
		return defaultTokenBudget
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultTokenBudget
	}
	return n
}

// Register registers the per-iteration handler for AgentLoopTask. It must be
// called before the engine's StartWorkers (HandleAgentLoop panics otherwise).
func (b *AgentLoopBuilder) Register() {
	b.eng.HandleAgentLoop(AgentLoopTask, func(ctx context.Context, lc *engine.AgentLoopContext) error {
		return b.handle(ctx, lc)
	})
}

// RegisterWorkflow declares the one-step agent-loop workflow with the engine.
// Call it after StartWorkers (mirroring credverify's register-after-start).
func (b *AgentLoopBuilder) RegisterWorkflow(ctx context.Context) error {
	def := engine.WorkflowDef{
		Name: AgentLoopWorkflow,
		Steps: []engine.StepDef{
			engine.AgentLoopStep(AgentLoopStepID, AgentLoopTask, engine.AgentLoopConfig{
				MaxIterations: b.maxIters,
			}),
		},
	}
	return b.eng.RegisterWorkflow(ctx, def)
}

// Dispatch triggers an ai-build run for sessionID in scope and returns the new
// run id. The console (next unit) calls this to start a durable build; the run
// id keys the stream subscription for live events.
func (b *AgentLoopBuilder) Dispatch(ctx context.Context, scope domain.Scope, sessionID string) (string, error) {
	raw, err := json.Marshal(runInput{
		SessionID: sessionID,
		OrgID:     scope.OrgID,
		EnvID:     scope.EnvID,
	})
	if err != nil {
		return "", fmt.Errorf("aibuilder: marshal run input: %w", err)
	}
	return b.eng.StartRun(ctx, AgentLoopWorkflow, raw)
}

// RunFailed reports whether runID has reached a terminal, non-successful state
// (failed / cancelled / compensated). The console polls it to resolve a stream
// that ended without a terminal event — the iteration-cap path fails the run
// with no KindError stream, visible only via the run snapshot. A non-terminal or
// still-running run reports false; a snapshot-fetch error is surfaced so the
// caller can fall back to its overall deadline rather than treat it as failed.
func (b *AgentLoopBuilder) RunFailed(ctx context.Context, runID string) (bool, error) {
	snap, err := b.eng.GetRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("aibuilder: get run %q: %w", runID, err)
	}
	return snap.IsTerminal() && !snap.Succeeded(), nil
}

// handle runs exactly one iteration of the builder loop. It loads the prior
// checkpoint (or initializes from the session on iteration 0), calls the model
// once, applies any tool calls to the draft, streams an event per call, and ends
// with one terminal action: Complete (the draft validated, persisted to the
// session) or Continue (more work, or a self-correct round after a compile
// failure). A model/transport error fails the iteration. The engine's
// MaxIterations bound caps a runaway self-correct.
func (b *AgentLoopBuilder) handle(ctx context.Context, lc loopContext) error {
	st, err := b.loadState(ctx, lc)
	if err != nil {
		return err
	}

	sys := systemPrompt(b.reg.Catalog(), st.GatedTypes)
	if st.Mode == store.BuilderSessionModeRefine && st.Original != nil {
		sys = refineSystemPrompt(b.reg.Catalog(), st.GatedTypes, st.Original)
	}
	tools := buildTools(st.GatedTypes)
	gatedSet := make(map[string]bool, len(st.GatedTypes))
	for _, t := range st.GatedTypes {
		gatedSet[t] = true
	}

	resp, err := b.client.Message(ctx, MessageRequest{
		Model:      b.model,
		System:     sys,
		Messages:   st.Messages,
		Tools:      tools,
		ToolChoice: "auto",
		MaxTokens:  DefaultMaxTokens,
	})
	if err != nil {
		slog.Error("aibuilder: model call failed", "sessionID", st.SessionID, "iter", lc.Iteration(), "err", err)
		b.emit(lc, Event{Kind: KindError, Err: err})
		return fmt.Errorf("aibuilder: model call: %w", err)
	}

	// Accumulate token spend and enforce the budget (when enabled) before doing
	// any more work this iteration — a run that has already spent its budget on
	// this call gets no further benefit from applying tool calls it can never
	// finish paying for. Mirrors the MaxIterations bound's role but with an
	// explicit KindError instead of a silent iteration-cap failure, since a
	// budget breach is diagnosable (cost) in a way "ran out of iterations" is not.
	st.TokensUsed += resp.Usage.InputTokens + resp.Usage.OutputTokens
	if b.tokenBudget > 0 && st.TokensUsed >= int64(b.tokenBudget) {
		budgetErr := fmt.Errorf("aibuilder: token budget exceeded (%d >= %d)", st.TokensUsed, b.tokenBudget)
		slog.Error("aibuilder: token budget exceeded", "sessionID", st.SessionID, "iter", lc.Iteration(), "tokensUsed", st.TokensUsed, "tokenBudget", b.tokenBudget)
		b.emit(lc, Event{Kind: KindError, Err: budgetErr, Draft: st.Draft.clone()})
		return budgetErr
	}

	// Record the assistant turn verbatim so the next iteration carries the
	// model's own tool_use blocks (the API requires tool_result to follow the
	// matching tool_use).
	st.Messages = append(st.Messages, Message{
		Role:      RoleAssistant,
		Text:      resp.Text,
		ToolCalls: resp.ToolCalls,
	})

	if resp.Text != "" {
		b.emit(lc, Event{Kind: KindReasoning, Text: resp.Text, Draft: st.Draft.clone()})
	}

	// No tool calls => the model believes it is done. Validate and finalize, or
	// feed the compile error back as a self-correct round on the next iteration.
	if len(resp.ToolCalls) == 0 {
		if vErr := validateDraft(st.Scope, &st.Draft); vErr != nil {
			if scErr := b.selfCorrectLimitExceeded(lc, st); scErr != nil {
				return scErr
			}
			st.Messages = append(st.Messages, Message{
				Role: RoleUser,
				Text: "The workflow failed validation: " + vErr.Error() +
					". Fix the offending step, then stop.",
			})
			return b.continueLoop(lc, st)
		}
		return b.finish(ctx, lc, st, resp.Text)
	}

	// Apply each tool call, streaming events and collecting results to feed back
	// to the model on the next iteration.
	var results []ToolResult
	var hadError bool
	for _, tc := range resp.ToolCalls {
		ev, content, applyErr := applyGatedToolCall(&st.Draft, b.reg, gatedSet, b.maxSteps, newStepID, tc)
		if applyErr != nil {
			hadError = true
			results = append(results, ToolResult{
				ToolCallID: tc.ID,
				Content:    applyErr.Error(),
				IsError:    true,
			})
			continue
		}
		switch ev.Kind {
		case KindReasoning:
			ev.Draft = st.Draft.clone()
			b.emit(lc, ev)
		case KindCanvas:
			b.emit(lc, Event{Kind: KindTool, ToolName: tc.Name, ToolCallID: tc.ID, Draft: st.Draft.clone()})
			ev.Draft = st.Draft.clone()
			b.emit(lc, ev)
		}
		results = append(results, ToolResult{ToolCallID: tc.ID, Content: content})
	}
	if hadError {
		if scErr := b.selfCorrectLimitExceeded(lc, st); scErr != nil {
			return scErr
		}
	} else {
		st.SelfCorrectAttempts = 0
	}
	st.Messages = append(st.Messages, Message{Role: RoleUser, ToolResults: results})
	return b.continueLoop(lc, st)
}

// loadState decodes the prior checkpoint, or builds the initial state from the
// run input on iteration 0: it loads the scope-filtered session for its prompt,
// computes the provider-gated step types, and seeds the conversation with the
// user prompt over an empty draft.
func (b *AgentLoopBuilder) loadState(ctx context.Context, lc loopContext) (*loopState, error) {
	if cp := lc.Checkpoint(); cp != nil {
		var st loopState
		if err := json.Unmarshal(cp, &st); err != nil {
			return nil, fmt.Errorf("aibuilder: decode checkpoint: %w", err)
		}
		return &st, nil
	}

	var in runInput
	if err := json.Unmarshal(lc.Input(), &in); err != nil {
		return nil, fmt.Errorf("aibuilder: decode run input: %w", err)
	}
	scope := domain.Scope{OrgID: in.OrgID, EnvID: in.EnvID}
	sess, err := b.sessions.Get(ctx, scope, in.SessionID)
	if err != nil {
		return nil, fmt.Errorf("aibuilder: load session %q: %w", in.SessionID, err)
	}
	integrations, err := b.ints.List(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("aibuilder: list integrations: %w", err)
	}
	st := &loopState{
		SessionID:  in.SessionID,
		Scope:      scope,
		GatedTypes: AvailableStepTypes(b.reg, integrations),
		Messages:   []Message{{Role: RoleUser, Text: b.turnInstruction(ctx, scope, in.SessionID, sess.Prompt)}},
		Draft:      WorkflowDraft{},
		Mode:       store.BuilderSessionModeCreate,
	}
	// Multi-turn seeding rule: if the session already holds a workflow, start from
	// it and freeze that shape as Original so the refine (edit) system prompt keeps
	// describing the pre-edit workflow across iterations. This covers BOTH a refine
	// session (seeded from an existing workflow at dispatch) AND a follow-up turn on
	// a create session whose prior turn produced steps — both must "continue editing
	// the current draft" rather than rebuild. A create session's FIRST turn holds
	// the empty-object placeholder, which decodes to a zero draft and is NOT a seed,
	// so it starts from empty with the create system prompt. The seeded
	// TriggerIdentifier is immutable — setWorkflowMetadata only writes it when empty,
	// so the model cannot repoint at a new identifier.
	seeded, ok, err := decodeSeedDraft(sess.DraftWorkflow)
	if err != nil {
		return nil, fmt.Errorf("aibuilder: decode seed draft: %w", err)
	}
	if ok {
		st.Mode = store.BuilderSessionModeRefine
		st.Draft = seeded
		st.Original = seeded.clone()
	}
	return st, nil
}

// turnInstruction returns the user instruction driving this run's first
// iteration: the latest user message in the session's chat history (a follow-up
// copilot turn), falling back to fallback (the original session prompt) when the
// history has no user message — the case for a session's first turn, whose
// instruction is carried by sess.Prompt rather than an appended message. A
// history read error is non-fatal: the fallback prompt still drives the run.
func (b *AgentLoopBuilder) turnInstruction(ctx context.Context, scope domain.Scope, sessionID, fallback string) string {
	msgs, err := b.sessions.Messages(ctx, scope, sessionID)
	if err != nil {
		return fallback
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == string(RoleUser) && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return fallback
}

// decodeSeedDraft decodes a session's stored draft and reports whether it already
// holds a workflow worth continuing from. The empty-object placeholder ("{}") a
// create session's first turn stores decodes to a zero draft and is NOT a seed;
// any draft carrying a name, trigger identifier, or steps IS.
func decodeSeedDraft(raw json.RawMessage) (WorkflowDraft, bool, error) {
	if len(raw) == 0 {
		return WorkflowDraft{}, false, nil
	}
	var d WorkflowDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		return WorkflowDraft{}, false, err
	}
	if d.Name == "" && d.TriggerIdentifier == "" && len(d.Steps) == 0 {
		return WorkflowDraft{}, false, nil
	}
	return d, true, nil
}

// finish persists the validated draft on the session (the durable result the run
// output cannot be trusted to carry), streams a terminal done event, and
// completes the agent-loop step.
func (b *AgentLoopBuilder) finish(ctx context.Context, lc loopContext, st *loopState, summary string) error {
	draftJSON, err := json.Marshal(st.Draft)
	if err != nil {
		return fmt.Errorf("aibuilder: marshal draft: %w", err)
	}
	// Persist the final validated draft AND flip to the terminal Ready status in
	// one write — the console's confirm (create) and apply (refine) endpoints only
	// act on a Ready session, so this is what makes the drafted workflow eligible
	// to be committed. The status arg of UpdateDraft carries it (no second call,
	// and the SessionStore interface stays minimal).
	if err := b.sessions.UpdateDraft(ctx, st.Scope, st.SessionID, draftJSON, store.BuilderSessionReady); err != nil {
		return fmt.Errorf("aibuilder: persist draft: %w", err)
	}
	b.emit(lc, Event{Kind: KindDone, Text: summary, Draft: st.Draft.clone()})
	return lc.Complete(draftJSON)
}

// selfCorrectLimitExceeded increments st.SelfCorrectAttempts and, once it
// reaches maxSelfCorrectAttempts, streams a terminal KindError event and
// returns the error the caller must return from handle — failing fast instead
// of riding a persistently wrong model toward the engine's MaxIterations bound.
// A nil return means the caller should proceed with its normal self-correct
// continuation (append the feedback message / tool results and Continue).
func (b *AgentLoopBuilder) selfCorrectLimitExceeded(lc loopContext, st *loopState) error {
	st.SelfCorrectAttempts++
	if st.SelfCorrectAttempts < maxSelfCorrectAttempts {
		return nil
	}
	err := fmt.Errorf("aibuilder: self-correction limit exceeded (%d consecutive invalid turns)", st.SelfCorrectAttempts)
	slog.Error("aibuilder: self-correction limit exceeded", "sessionID", st.SessionID, "attempts", st.SelfCorrectAttempts)
	b.emit(lc, Event{Kind: KindError, Err: err, Draft: st.Draft.clone()})
	return err
}

// continueLoop checkpoints the full loop state and asks the engine for another
// iteration.
func (b *AgentLoopBuilder) continueLoop(lc loopContext, st *loopState) error {
	cp, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("aibuilder: marshal checkpoint: %w", err)
	}
	return lc.Continue(cp)
}

// emit streams ev to the run's stream subject as JSON. Streaming is
// fire-and-forget (the console subscribes opportunistically); a publish failure
// must not derail the build, so the error is dropped.
func (b *AgentLoopBuilder) emit(lc loopContext, ev Event) {
	se := streamEvent{
		Kind:       ev.Kind,
		Text:       ev.Text,
		ToolName:   ev.ToolName,
		ToolCallID: ev.ToolCallID,
		Draft:      ev.Draft,
	}
	if ev.Err != nil {
		se.Error = ev.Err.Error()
	}
	data, err := json.Marshal(se)
	if err != nil {
		return
	}
	_ = lc.Stream(data)
}
