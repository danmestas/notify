package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// aiGenerateTimeout caps the total wall time for one AI generation round-trip
// (up to 24 LLM calls). A slow or wedged model hangs the handler indefinitely
// without this guard.
const aiGenerateTimeout = 90 * time.Second

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
}

// handleWorkflowAIGenerate handles POST /console/api/workflows/ai-generate. It
// creates a builder session, runs the AI builder synchronously over a fresh
// draft (collecting the event stream into the session's message history),
// stores the validated draft, and patches a preview fragment for review. A
// builder failure renders inline, never a 500.
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
	if c.aiBuilder == nil || c.deps.BuilderSessions == nil {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Error: "AI builder not configured. Set ANTHROPIC_API_KEY to enable the Guided tab.",
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

	sess, err := c.deps.BuilderSessions.Create(r.Context(), scope, prompt)
	if err != nil {
		c.deps.Logger.Error("console: ai create session", "err", err)
		c.serveError(w, r)
		return
	}

	// Run the builder synchronously, collecting events into the session history.
	var (
		draft     aibuilder.WorkflowDraft
		reasoning []string
	)
	emit := func(ev aibuilder.Event) {
		switch ev.Kind {
		case aibuilder.KindReasoning:
			if ev.Text != "" {
				reasoning = append(reasoning, ev.Text)
				_ = c.deps.BuilderSessions.AppendMessage(r.Context(), scope, sess.ID, "assistant", ev.Text, "")
			}
		case aibuilder.KindTool:
			_ = c.deps.BuilderSessions.AppendMessage(r.Context(), scope, sess.ID, "tool", ev.ToolName, "")
		}
	}

	timeout := aiGenerateTimeout
	if c.aiGenerateTimeoutOverride > 0 {
		timeout = c.aiGenerateTimeoutOverride
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	runErr := c.aiBuilder.Run(ctx, scope, &draft, prompt, emit)
	if runErr != nil {
		msg := "The builder could not produce a valid workflow: " + runErr.Error()
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
			msg = "Generation timed out — please try again."
		}
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Reasoning: reasoning,
			Error:     msg,
		})
		return
	}

	// Persist the validated draft on the session for the confirm step.
	draftJSON, err := json.Marshal(draft)
	if err != nil {
		c.deps.Logger.Error("console: ai marshal draft", "err", err)
		c.serveError(w, r)
		return
	}
	if err := c.deps.BuilderSessions.UpdateDraft(r.Context(), scope, sess.ID, draftJSON, store.BuilderSessionDraft); err != nil {
		c.deps.Logger.Error("console: ai store draft", "err", err)
		c.serveError(w, r)
		return
	}

	c.patchAIPreview(w, r, c.aiPreviewFromDraft(sess.ID, &draft, reasoning))
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

	// Idempotency guard: a session that is already confirmed or discarded must not
	// re-run Create, which would surface a spurious "identifier already exists" error.
	if sess.Status != store.BuilderSessionDraft {
		msg := "This workflow was already confirmed."
		if sess.Status == store.BuilderSessionDiscarded {
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
	for i, st := range draft.Steps {
		label := st.Type
		if def := c.stepTypes.Get(st.Type); def != nil {
			label = def.DisplayName()
		}
		v.Steps = append(v.Steps, aiBuilderStepView{
			Index:   i + 1,
			Type:    st.Type,
			Label:   label,
			Name:    st.Name,
			Summary: c.aiStepSummary(st.Type, st.ControlValues),
		})
	}
	return v
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
