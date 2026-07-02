package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// aiGenerateTimeout caps the total wall time the ai-generate handler holds the
// SSE stream open for one durable build run (up to DefaultMaxIterations model
// turns). The iteration-cap path fails the run with no terminal stream event, so
// this deadline is the backstop that resolves the request rather than hanging.
const aiGenerateTimeout = 90 * time.Second

// aiBuildPollInterval is how often the ai-generate handler polls the run status
// while streaming, to resolve a run that ended (failed) without a terminal stream
// event promptly rather than waiting out aiGenerateTimeout.
const aiBuildPollInterval = 3 * time.Second

// aiBuilderCreateSuggestions are the example-prompt cards shown in the Guided
// create tab. They represent common, real-world workflow patterns so operators
// can start with a single click rather than composing a prompt from scratch.
var aiBuilderCreateSuggestions = []string{
	"Welcome series: send an in-app greeting immediately after signup, wait 2 days, then email onboarding tips",
	"Payment failed: email the customer right away, wait 3 days, send an SMS retry reminder",
	"Weekly digest: every Sunday collect the last 7 days of unread in-app notifications and send an email summary",
	"Re-engagement: if a user hasn't logged in for 14 days, send an in-app nudge then an email 2 days later",
}

// aiBuilderRefineSuggestions are the example-prompt cards shown in the
// Refine-with-AI sidebar on the workflow detail page.
var aiBuilderRefineSuggestions = []string{
	"Add an SMS reminder 24 hours after the first email if the user hasn't opened it",
	"Insert a 2-day delay between the first and second steps",
	"Change the email subject to mention the user's first name",
	"Add a final in-app follow-up at the end of the workflow",
}

// aiBuilderStepView is one proposed step in the AI builder preview: its type
// label, optional name, and a short summary of the salient control values.
type aiBuilderStepView struct {
	Index   int
	Type    string
	Label   string
	Name    string
	Summary string
}

// aiBuilderPreviewView binds the ai-builder-preview fragment. Exactly one of
// the proposed workflow (SessionID + Name + Steps) or Error is meaningful: an
// Error renders the inline failure state, otherwise the preview renders with a
// "Use this workflow" / "Discard" action pair.
type aiBuilderPreviewView struct {
	SessionID         string
	Name              string
	TriggerIdentifier string
	Steps             []aiBuilderStepView
	Reasoning         []string
	Error             string
	CSRF              string
	// Mode is "" / "create" for the create-tab preview (Use this workflow →
	// ai-confirm) or "refine" for the detail-page copilot (Apply changes →
	// ai-apply/{slug}). Slug is the refine target's trigger identifier, used to
	// build the apply action URL. Both are empty in create mode.
	Mode string
	Slug string
}

// aiBuilderCanvasView binds the ai-builder-canvas fragment: the workflow taking
// shape live as the model emits canvas events (its name so far plus its ordered
// steps). Re-rendered in place on every canvas/tool stream event.
type aiBuilderCanvasView struct {
	Name  string
	Steps []aiBuilderStepView
}

// aiStreamEvent is the console-side decode of one builder Event published on
// stream.<runID>.build. It mirrors aibuilder's on-the-wire streamEvent (which is
// unexported): kind routes the patch, Draft snapshots the workflow at emit time.
type aiStreamEvent struct {
	Kind  string                   `json:"kind"`
	Text  string                   `json:"text"`
	Draft *aibuilder.WorkflowDraft `json:"draft"`
	Error string                   `json:"error"`
}

// handleWorkflowAIGenerate handles POST /console/api/workflows/ai-generate. It
// creates a builder session, dispatches a durable agent-loop build run, and
// holds an SSE connection open relaying the run's live Events: reasoning lines
// append to #ai-builder-reasoning, canvas snapshots re-render #ai-builder-canvas,
// and the terminal done event renders the final preview into #ai-builder-preview.
// A model/transport error event, a failed run, or the overall deadline each
// resolve the stream with an inline failure — never a hung connection or a 500.
func (c *Console) handleWorkflowAIGenerate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowAIGenerate: w is nil")
	}
	if r == nil {
		panic("handleWorkflowAIGenerate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIGenerate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.aiBuilder == nil || c.buildStream == nil || c.deps.BuilderSessions == nil {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Error: "AI builder not configured. Set NOTIFY_AI_API_KEY (or ANTHROPIC_API_KEY) to enable the Guided tab.",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Error: "Describe the workflow you want before generating.",
		})
		return
	}
	if len([]rune(prompt)) > aiPromptMaxLen {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Error: "Prompt is too long — please keep it under 2 000 characters.",
		})
		return
	}
	if !c.aiLimiter.Allow(scope) {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Error: "You're generating a lot right now — wait a moment and try again.",
		})
		return
	}

	sess, err := c.deps.BuilderSessions.Create(r.Context(), scope, prompt)
	if err != nil {
		c.deps.Logger.Error("console: ai create session", "err", err)
		c.serveError(w, r)
		return
	}

	runID, err := c.aiBuilder.Dispatch(r.Context(), scope, sess.ID)
	if err != nil {
		c.deps.Logger.Error("console: ai dispatch run", "err", err)
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Error:     "Could not start the builder. Please try again.",
		})
		return
	}

	c.streamAIBuild(w, r, scope, sess.ID, runID, store.BuilderSessionModeCreate, "")
}

// streamAIBuild subscribes to the run's build stream and relays each Event to the
// browser as a Datastar patch until a terminal event, a failed run, the overall
// deadline, or a client disconnect resolves it. One SSE generator carries every
// patch (reasoning appends, canvas re-renders, the final preview).
func (c *Console) streamAIBuild(w http.ResponseWriter, r *http.Request, scope domain.Scope, sessionID, runID, mode, slug string) {
	timeout := aiGenerateTimeout
	if c.aiGenerateTimeoutOverride > 0 {
		timeout = c.aiGenerateTimeoutOverride
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Buffered so the bus callback never blocks; a slow client drops events
	// rather than stalling the NATS delivery goroutine (live_tail idiom).
	events := make(chan []byte, liveTailBuffer)
	unsub, err := c.buildStream.SubscribeBuildStream(runID, func(d []byte) {
		cp := make([]byte, len(d))
		copy(cp, d)
		select {
		case events <- cp:
		default: // buffer full — drop; the canvas/preview reconciles on the next event.
		}
	})
	if err != nil {
		c.deps.Logger.Error("console: ai subscribe build stream", "err", err)
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sessionID,
			Error:     "Could not start the builder. Please try again.",
		})
		return
	}
	defer func() { _ = unsub() }()

	poll := aiBuildPollInterval
	if c.aiBuildPollOverride > 0 {
		poll = c.aiBuildPollOverride
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	sse := datastar.NewSSE(w, r)
	for {
		select {
		case <-ctx.Done():
			// Overall deadline (or client disconnect): check whether the run
			// actually completed (terminal event was dropped or arrived after the
			// context expired). Use a fresh background context for the session
			// lookup — r.Context() and ctx may both be cancelled at this point.
			lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			sess, sessErr := c.deps.BuilderSessions.Get(lookupCtx, scope, sessionID)
			lookupCancel()
			if sessErr == nil && sess.Status == store.BuilderSessionReady {
				// Run completed; the terminal event was dropped or arrived late.
				// Serve the real draft so the user sees their result.
				c.emitAICanvas(sse, aiBuilderCanvasView{})
				c.emitAIPreview(sse, r, c.finalPreviewView(r, scope, sessionID, mode, slug, nil))
				return
			}
			c.emitAIPreview(sse, r, aiBuilderPreviewView{
				SessionID: sessionID,
				Mode:      mode,
				Slug:      slug,
				Error:     "Generation did not finish — please try again.",
			})
			return
		case raw, ok := <-events:
			if !ok {
				return
			}
			if done := c.relayAIEvent(sse, r, scope, sessionID, mode, slug, raw); done {
				return
			}
		case <-ticker.C:
			if failed, err := c.aiBuilder.RunFailed(ctx, runID); err == nil && failed {
				c.emitAIPreview(sse, r, aiBuilderPreviewView{
					SessionID: sessionID,
					Mode:      mode,
					Slug:      slug,
					Error:     "Generation did not finish — please try again.",
				})
				return
			}
		}
	}
}

// relayAIEvent decodes one streamed Event and emits its patch. It returns true
// when the event is terminal (done → final preview, error → inline failure) and
// the stream should end; false to keep relaying. An undecodable payload is
// dropped (defensive — never fail the stream on a malformed event).
func (c *Console) relayAIEvent(sse *datastar.ServerSentEventGenerator, r *http.Request, scope domain.Scope, sessionID, mode, slug string, raw []byte) bool {
	var ev aiStreamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		c.deps.Logger.Debug("console: ai stream decode", "err", err)
		return false
	}
	switch aibuilder.EventKind(ev.Kind) {
	case aibuilder.KindReasoning:
		if ev.Text != "" {
			c.emitAIReasoning(sse, ev.Text)
		}
		return false
	case aibuilder.KindTool, aibuilder.KindCanvas:
		if ev.Draft != nil {
			c.emitAICanvas(sse, c.aiCanvasFromDraft(ev.Draft))
		}
		return false
	case aibuilder.KindDone:
		// Clear the live canvas before rendering the final preview so the
		// workflow does not appear twice (canvas + preview).
		c.emitAICanvas(sse, aiBuilderCanvasView{})
		c.emitAIPreview(sse, r, c.finalPreviewView(r, scope, sessionID, mode, slug, ev.Draft))
		return true
	case aibuilder.KindError:
		// Log at Error level so the failure is visible in notify.log; the
		// model/transport detail lives in the log rather than the UI.
		c.deps.Logger.Error("console: ai build error event", "err", ev.Error)
		c.emitAIPreview(sse, r, aiBuilderPreviewView{
			SessionID: sessionID,
			Mode:      mode,
			Slug:      slug,
			Error:     "The AI provider returned an error. Please try again.",
		})
		return true
	default:
		return false
	}
}

// finalPreviewView builds the completion preview. The durable run persists the
// validated draft on the session (finish → UpdateDraft) before streaming done, so
// the session is the source of truth; the done event's draft is a defensive
// fallback if the session read is empty.
func (c *Console) finalPreviewView(r *http.Request, scope domain.Scope, sessionID, mode, slug string, fallback *aibuilder.WorkflowDraft) aiBuilderPreviewView {
	var draft aibuilder.WorkflowDraft
	if sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID); err == nil && len(sess.DraftWorkflow) > 0 {
		if uerr := json.Unmarshal(sess.DraftWorkflow, &draft); uerr != nil {
			c.deps.Logger.Debug("console: ai final draft decode", "err", uerr)
		}
	}
	if len(draft.Steps) == 0 && fallback != nil {
		draft = *fallback
	}
	v := c.aiPreviewFromDraft(sessionID, &draft, nil)
	v.Mode = mode
	v.Slug = slug
	return v
}

// handleWorkflowAIConfirm handles POST /console/api/workflows/ai-confirm/{sessionID}.
// It loads the scope-checked draft, compiles + persists it through the authoring
// service, marks the session confirmed, and redirects to the new workflow. A
// compile failure (defensive — the draft was validated at generate time)
// surfaces inline.
func (c *Console) handleWorkflowAIConfirm(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowAIConfirm: w is nil")
	}
	if r == nil {
		panic("handleWorkflowAIConfirm: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIConfirm: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.BuilderSessions == nil {
		c.serveNotFound(w, r)
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}

	sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai confirm load session", "err", err)
		c.serveError(w, r)
		return
	}

	// Only a session the durable run has finished (Ready) may be confirmed. A
	// still-running (Draft) session would confirm a partial/original draft; an
	// already-confirmed or discarded one must not re-run Create (which would
	// surface a spurious "identifier already exists" error). Every non-Ready status
	// is rejected inline.
	if sess.Status != store.BuilderSessionReady {
		msg := "This workflow is still being generated — try again in a moment."
		switch sess.Status {
		case store.BuilderSessionConfirmed:
			msg = "This workflow was already confirmed."
		case store.BuilderSessionDiscarded:
			msg = "This workflow draft was discarded."
		}
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Error:     msg,
		})
		return
	}

	var draft aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &draft); err != nil {
		c.deps.Logger.Error("console: ai confirm decode draft", "err", err)
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Error:     "The drafted workflow could not be read. Generate again.",
		})
		return
	}

	wf, err := c.deps.Authoring.Create(r.Context(), scope, authoring.WorkflowInput{
		Name:              draft.Name,
		TriggerIdentifier: draft.TriggerIdentifier,
		Steps:             draft.Steps,
	})
	if err != nil {
		var compileErr *authoring.ErrCompileFailure
		if errors.As(err, &compileErr) {
			c.patchAIPreview(w, r, c.aiPreviewFromDraft(sess.ID, &draft, nil, "Step configuration is invalid: "+compileErr.Error()))
			return
		}
		if errors.Is(err, authoring.ErrIdentifierConflict) {
			c.patchAIPreview(w, r, c.aiPreviewFromDraft(sess.ID, &draft, nil, "A workflow with trigger identifier \""+draft.TriggerIdentifier+"\" already exists."))
			return
		}
		c.deps.Logger.Error("console: ai confirm create", "err", err)
		c.patchAIPreview(w, r, c.aiPreviewFromDraft(sess.ID, &draft, nil, "Create failed."))
		return
	}
	if err := c.deps.Authoring.Publish(r.Context(), scope, wf.TriggerIdentifier); err != nil {
		c.deps.Logger.Error("console: ai confirm publish", "err", err)
		// The row exists; the editor is reachable. Continue to it.
	}
	if err := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sess.ID, store.BuilderSessionConfirmed); err != nil {
		c.deps.Logger.Error("console: ai confirm set status", "err", err)
	}
	http.Redirect(w, r, "/console/workflows/"+wf.TriggerIdentifier, http.StatusSeeOther)
}

// handleWorkflowAIDiscard handles POST /console/api/workflows/ai-discard/{sessionID}.
// It drops the drafted workflow (marks the session discarded) and clears the
// preview region.
func (c *Console) handleWorkflowAIDiscard(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowAIDiscard: w is nil")
	}
	if r == nil {
		panic("handleWorkflowAIDiscard: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIDiscard: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.BuilderSessions == nil {
		c.serveNotFound(w, r)
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	if err := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sessionID, store.BuilderSessionDiscarded); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.deps.Logger.Error("console: ai discard", "err", err)
	}
	// Clear the preview region; an empty session id yields a placeholder-only
	// fragment.
	c.patchAIPreview(w, r, aiBuilderPreviewView{})
}

// aiPreviewFromDraft builds the preview view from a draft. An optional error
// string (variadic, first value used) renders the inline failure banner above
// the otherwise-unchanged preview.
func (c *Console) aiPreviewFromDraft(sessionID string, draft *aibuilder.WorkflowDraft, reasoning []string, errMsg ...string) aiBuilderPreviewView {
	v := aiBuilderPreviewView{
		SessionID:         sessionID,
		Name:              draft.Name,
		TriggerIdentifier: draft.TriggerIdentifier,
		Reasoning:         reasoning,
	}
	if len(errMsg) > 0 {
		v.Error = errMsg[0]
	}
	v.Steps = c.aiStepViews(draft.Steps)
	return v
}

// aiCanvasFromDraft builds the live-canvas view from a draft snapshot: the
// workflow's name so far plus its ordered steps.
func (c *Console) aiCanvasFromDraft(draft *aibuilder.WorkflowDraft) aiBuilderCanvasView {
	return aiBuilderCanvasView{
		Name:  draft.Name,
		Steps: c.aiStepViews(draft.Steps),
	}
}

// aiStepViews projects draft steps into the index/label/name/summary rows the
// preview and canvas fragments both render.
func (c *Console) aiStepViews(steps []domain.Step) []aiBuilderStepView {
	if len(steps) == 0 {
		return nil
	}
	out := make([]aiBuilderStepView, 0, len(steps))
	for i, st := range steps {
		label := st.Type
		if def := c.stepTypes.Get(st.Type); def != nil {
			label = def.DisplayName()
		}
		out = append(out, aiBuilderStepView{
			Index:   i + 1,
			Type:    st.Type,
			Label:   label,
			Name:    st.Name,
			Summary: c.aiStepSummary(st.Type, st.ControlValues),
		})
	}
	return out
}

// aiStepSummary renders a short, human-readable summary of a step's salient
// control values for the preview (the full controls live in the editor after
// confirm). It prefers the type's headline field, falling back to the body.
func (c *Console) aiStepSummary(typ string, cv json.RawMessage) string {
	def := c.stepTypes.Get(typ)
	if def == nil || len(cv) == 0 {
		return ""
	}
	m := def.ControlMap(cv)
	for _, key := range []string{"subject", "title", "body", "url", "type", "limit", "dynamicKey"} {
		if v := strings.TrimSpace(m[key]); v != "" {
			return key + ": " + truncate(v, 80)
		}
	}
	return ""
}

// truncate shortens s to at most n runes, appending an ellipsis when clipped.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// patchAIPreview renders and streams the ai-builder-preview fragment, wiring the
// per-session CSRF token for the confirm/discard actions.
func (c *Console) patchAIPreview(w http.ResponseWriter, r *http.Request, v aiBuilderPreviewView) {
	if s, ok := sessionFromContext(r.Context()); ok {
		v.CSRF = s.CSRF
	}
	html, err := renderFragment(c.ts.base, "ai-builder-preview", v)
	if err != nil {
		c.deps.Logger.Error("console: render ai preview fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// emitAIReasoning appends one chain-of-thought line into #ai-builder-reasoning on
// the live SSE generator (the running log grows as the model narrates its plan).
func (c *Console) emitAIReasoning(sse *datastar.ServerSentEventGenerator, text string) {
	html, err := renderFragment(c.ts.base, "ai-builder-reasoning-line", text)
	if err != nil {
		c.deps.Logger.Error("console: render ai reasoning line", "err", err)
		return
	}
	if err := sse.PatchElements(
		html,
		datastar.WithSelector("#ai-builder-reasoning"),
		datastar.WithMode(datastar.ElementPatchModeAppend),
	); err != nil {
		c.deps.Logger.Debug("console: ai reasoning patch", "err", err)
	}
}

// emitAICanvas re-renders #ai-builder-canvas in place from the latest draft
// snapshot (the workflow building live). The fragment's outer #ai-builder-canvas
// element makes the default outer-mode patch line up.
func (c *Console) emitAICanvas(sse *datastar.ServerSentEventGenerator, v aiBuilderCanvasView) {
	html, err := renderFragment(c.ts.base, "ai-builder-canvas", v)
	if err != nil {
		c.deps.Logger.Error("console: render ai canvas fragment", "err", err)
		return
	}
	if err := sse.PatchElements(html); err != nil {
		c.deps.Logger.Debug("console: ai canvas patch", "err", err)
	}
}

// emitAIPreview renders the ai-builder-preview fragment (final preview or inline
// failure) on the live SSE generator, wiring the per-session CSRF token for the
// confirm/discard actions. The outer #ai-builder-preview element drives the patch.
func (c *Console) emitAIPreview(sse *datastar.ServerSentEventGenerator, r *http.Request, v aiBuilderPreviewView) {
	if s, ok := sessionFromContext(r.Context()); ok {
		v.CSRF = s.CSRF
	}
	html, err := renderFragment(c.ts.base, "ai-builder-preview", v)
	if err != nil {
		c.deps.Logger.Error("console: render ai preview fragment", "err", err)
		return
	}
	if err := sse.PatchElements(html); err != nil {
		c.deps.Logger.Debug("console: ai preview patch", "err", err)
	}
}
