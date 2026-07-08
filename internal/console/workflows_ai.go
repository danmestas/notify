package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// aiGenerateTimeout caps the total wall time a copilot turn holds the SSE stream
// open for one durable build run (up to DefaultMaxIterations model turns). The
// iteration-cap path fails the run with no terminal stream event, so this
// deadline is the backstop that resolves the request rather than hanging.
const aiGenerateTimeout = 90 * time.Second

// aiBuildPollInterval is how often a copilot turn polls the run status while
// streaming, to resolve a run that ended (failed) without a terminal stream
// event promptly rather than waiting out aiGenerateTimeout.
const aiBuildPollInterval = 3 * time.Second

// aiBuilderCreateSuggestions are the example-prompt cards shown in the copilot
// panel's empty state when building a NEW workflow. They represent common,
// real-world workflow patterns so operators can start with a single click rather
// than composing a prompt from scratch.
var aiBuilderCreateSuggestions = []string{
	"Welcome series: send an in-app greeting immediately after signup, wait 2 days, then email onboarding tips",
	"Payment failed: email the customer right away, wait 3 days, send an SMS retry reminder",
	"Weekly digest: every Sunday collect the last 7 days of unread in-app notifications and send an email summary",
	"Re-engagement: if a user hasn't logged in for 14 days, send an in-app nudge then an email 2 days later",
}

// aiBuilderRefineSuggestions are the example-prompt cards shown in the copilot
// panel's empty state when refining an EXISTING workflow (the builder view opened
// via ?slug=).
var aiBuilderRefineSuggestions = []string{
	"Add an SMS reminder 24 hours after the first email if the user hasn't opened it",
	"Insert a 2-day delay between the first and second steps",
	"Change the email subject to mention the user's first name",
	"Add a final in-app follow-up at the end of the workflow",
}

// aiBuilderStepView is one proposed step in the AI builder canvas: its type
// label, optional name, and a short summary of the salient control values.
type aiBuilderStepView struct {
	Index   int
	Type    string
	Label   string
	Name    string
	Summary string
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
	// ToolName / ToolCallID accompany a KindTool event: the copilot relay keys a
	// per-tool-call chip on ToolCallID and labels it from ToolName.
	ToolName   string `json:"toolName"`
	ToolCallID string `json:"toolCallId"`
}

// handleWorkflowAIGenerate handles POST /console/api/workflows/ai-generate — the
// copilot's FIRST turn when building a brand-new workflow. It creates a builder
// session, records the user's opening instruction, snapshots the (empty) pre-turn
// draft as a checkpoint, marks the session in-flight, dispatches a durable
// agent-loop build run, and streams the run's live events into a fresh assistant
// turn in the docked #ai-thread. Follow-ups go to the session's /message endpoint.
// A model/transport error event, a failed run, or the overall deadline each
// resolve the stream inline — never a hung connection or a 500.
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
		c.patchAIError(w, r, "AI builder not configured. Set NOTIFY_AI_API_KEY (or ANTHROPIC_API_KEY) to enable the copilot.")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		c.patchAIError(w, r, "Describe the workflow you want before generating.")
		return
	}
	if len([]rune(prompt)) > aiPromptMaxLen {
		c.patchAIError(w, r, "Prompt is too long — please keep it under 2 000 characters.")
		return
	}
	if !c.aiLimiter.Allow(scope) {
		c.patchAIError(w, r, "You're generating a lot right now — wait a moment and try again.")
		return
	}

	sess, err := c.deps.BuilderSessions.Create(r.Context(), scope, prompt)
	if err != nil {
		c.deps.Logger.Error("console: ai create session", "err", err)
		c.serveError(w, r)
		return
	}

	// Per-turn setup runs BEFORE Dispatch so the first turn is Discard/Try-again
	// capable and guarded, and so the in-flight marker is not clobbered by the
	// run's own terminal write: record the user turn, snapshot the KNOWN-empty
	// draft the session was created with (not a re-read) so a fast run cannot race
	// steps into the first turn's undo point, then mark the session Generating.
	msgID, aerr := c.deps.BuilderSessions.AppendMessage(r.Context(), scope, sess.ID, "user", prompt, "")
	if aerr != nil {
		c.deps.Logger.Error("console: ai copilot create append message", "err", aerr)
	}
	if _, cerr := c.deps.BuilderSessions.PushCheckpoint(r.Context(), scope, sess.ID, msgID, sess.DraftWorkflow); cerr != nil {
		c.deps.Logger.Error("console: ai copilot create push checkpoint", "err", cerr)
	}
	if serr := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sess.ID, store.BuilderSessionGenerating); serr != nil {
		c.deps.Logger.Error("console: ai copilot create set status", "err", serr)
	}

	runID, err := c.aiBuilder.Dispatch(r.Context(), scope, sess.ID)
	if err != nil {
		c.deps.Logger.Error("console: ai dispatch run", "err", err)
		c.resetInFlight(scope, sess.ID)
		c.patchAIError(w, r, "Could not start the builder. Please try again.")
		return
	}

	c.streamAICopilotTurn(w, r, scope, sess.ID, runID, prompt, msgID, true, false)
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
// canvas fragment renders.
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
// control values for the canvas. It prefers the type's headline field, falling
// back to the body.
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

// patchAIError renders the ai-copilot-note fragment carrying a user-facing error
// and patches it into the copilot panel's #ai-copilot-note region. Used by the
// copilot handlers' pre-turn rejections (not configured, empty/over-long prompt,
// rate limited, dispatch failed) — one-shot HTTP responses, not the live SSE
// stream.
func (c *Console) patchAIError(w http.ResponseWriter, r *http.Request, msg string) {
	html, err := renderFragment(c.ts.base, "ai-copilot-note", msg)
	if err != nil {
		c.deps.Logger.Error("console: render ai copilot note", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
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
