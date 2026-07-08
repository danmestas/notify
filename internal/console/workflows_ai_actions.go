package console

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// handleWorkflowAIKeep handles POST /console/api/workflows/ai/{sessionID}/keep —
// the per-turn "Keep all" action. It persists the session's current draft as a
// real workflow: a create session goes through authoring.Create (the ai-confirm
// path), a refine session through authoring.Update against its target slug (the
// ai-apply path). On success it marks the session confirmed and patches the
// turn's action bar to a "Saved · Open workflow" state linking to the workflow.
// The Ready guard mirrors ai-confirm/ai-apply: a still-generating or already
// terminal session is rejected inline in the action bar.
func (c *Console) handleWorkflowAIKeep(w http.ResponseWriter, r *http.Request) {
	if w == nil || r == nil {
		panic("handleWorkflowAIKeep: nil w or r")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIKeep: no scope in context")
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	turnID := r.FormValue("msg_id")

	sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai keep load session", "err", err)
		c.serveError(w, r)
		return
	}

	// Only a session the durable run has finished (Ready) may be kept — the same
	// invariant ai-confirm/ai-apply hold. Every other status is a no-op with an
	// inline explanation in the action bar.
	if sess.Status != store.BuilderSessionReady {
		msg := "Still generating — try again in a moment."
		switch sess.Status {
		case store.BuilderSessionConfirmed:
			msg = "Already saved."
		case store.BuilderSessionDiscarded:
			msg = "This draft was discarded."
		}
		c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: turnID, CSRF: csrfFromRequest(r), Error: msg})
		return
	}

	var draft aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &draft); err != nil {
		c.deps.Logger.Error("console: ai keep decode draft", "err", err)
		c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: turnID, CSRF: csrfFromRequest(r), Error: "The draft could not be read."})
		return
	}

	slug, actErr := c.keepPersist(r, scope, sess, &draft)
	if actErr != "" {
		c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: turnID, CSRF: csrfFromRequest(r), Error: actErr})
		return
	}

	if err := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sessionID, store.BuilderSessionConfirmed); err != nil {
		c.deps.Logger.Error("console: ai keep set status", "err", err)
	}
	c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: turnID, CSRF: csrfFromRequest(r), Saved: true, Slug: slug})
}

// keepPersist commits the draft as a workflow, routing on the session's mode:
// create → authoring.Create (+ Publish), refine → authoring.Update against the
// session's target slug (+ Publish). It returns the workflow's slug on success,
// or a user-facing error string to render inline. Mirrors the create/apply
// handlers' error mapping so Keep behaves identically to the terminal actions.
func (c *Console) keepPersist(r *http.Request, scope domain.Scope, sess store.BuilderSession, draft *aibuilder.WorkflowDraft) (slug, errMsg string) {
	if sess.Mode == store.BuilderSessionModeRefine {
		target := sess.TargetSlug
		if target == "" {
			return "", "This refine session has no target workflow."
		}
		if _, err := c.deps.Authoring.Update(r.Context(), scope, target, authoring.UpdateInput{
			Name:          draft.Name,
			Steps:         draft.Steps,
			PayloadSchema: draft.PayloadSchema,
		}); err != nil {
			var compileErr *authoring.ErrCompileFailure
			if errors.As(err, &compileErr) {
				return "", "Step configuration is invalid: " + compileErr.Error()
			}
			if isNotFound(err) {
				return "", "The target workflow no longer exists."
			}
			c.deps.Logger.Error("console: ai keep update", "err", err)
			return "", "Saving the changes failed."
		}
		if err := c.deps.Authoring.Publish(r.Context(), scope, target); err != nil {
			c.deps.Logger.Error("console: ai keep publish (refine)", "err", err)
		}
		return target, ""
	}

	wf, err := c.deps.Authoring.Create(r.Context(), scope, authoring.WorkflowInput{
		Name:              draft.Name,
		TriggerIdentifier: draft.TriggerIdentifier,
		Steps:             draft.Steps,
	})
	if err != nil {
		var compileErr *authoring.ErrCompileFailure
		if errors.As(err, &compileErr) {
			return "", "Step configuration is invalid: " + compileErr.Error()
		}
		if errors.Is(err, authoring.ErrIdentifierConflict) {
			return "", "A workflow with trigger identifier \"" + draft.TriggerIdentifier + "\" already exists."
		}
		c.deps.Logger.Error("console: ai keep create", "err", err)
		return "", "Saving the workflow failed."
	}
	if err := c.deps.Authoring.Publish(r.Context(), scope, wf.TriggerIdentifier); err != nil {
		c.deps.Logger.Error("console: ai keep publish (create)", "err", err)
	}
	return wf.TriggerIdentifier, ""
}

// handleWorkflowAIDiscardTurn handles POST
// /console/api/workflows/ai/{sessionID}/discard-turn — the per-turn "Discard"
// action. It reverts the draft to the checkpoint taken before the latest turn
// (Slice 1 pushes it pre-dispatch), removes the turn's messages, re-renders the
// canvas from the restored draft, drops the turn's thread elements, and restores
// the now-latest turn's action bar. Scope-checked; a session with no checkpoint
// (nothing to undo) is a graceful inline no-op.
func (c *Console) handleWorkflowAIDiscardTurn(w http.ResponseWriter, r *http.Request) {
	if w == nil || r == nil {
		panic("handleWorkflowAIDiscardTurn: nil w or r")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIDiscardTurn: no scope in context")
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	formTurnID := r.FormValue("msg_id")

	if _, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai discard-turn load session", "err", err)
		c.serveError(w, r)
		return
	}

	cp, err := c.deps.BuilderSessions.LatestCheckpoint(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: formTurnID, CSRF: csrfFromRequest(r), Error: "Nothing to undo."})
			return
		}
		c.deps.Logger.Error("console: ai discard-turn checkpoint", "err", err)
		c.serveError(w, r)
		return
	}
	turnID := cp.MessageID

	// Compute the previous turn BEFORE deleting this one, so we can restore its
	// action bar as the new latest turn.
	prevTurnID := c.previousUserTurnID(r.Context(), scope, sessionID, turnID)

	// Pop the checkpoint: reverts the draft to the pre-turn snapshot AND removes
	// the checkpoint (this turn is gone for good), leaving the session Ready.
	if _, err := c.deps.BuilderSessions.PopCheckpoint(r.Context(), scope, sessionID); err != nil {
		c.deps.Logger.Error("console: ai discard-turn pop", "err", err)
		c.serveError(w, r)
		return
	}
	// Drop the turn's messages (the user instruction + its assistant reply) so the
	// thread and the restored draft stay consistent across a reload.
	if err := c.deps.BuilderSessions.DeleteMessagesFrom(r.Context(), scope, sessionID, turnID, true); err != nil {
		c.deps.Logger.Error("console: ai discard-turn delete messages", "err", err)
	}

	sse := datastar.NewSSE(w, r)
	// Re-render the canvas from the restored draft (empty draft clears it).
	c.emitAICanvas(sse, c.copilotFinalCanvas(r, scope, sessionID, nil))
	// Drop the reverted turn's thread elements (assistant block + user bubble).
	c.removeTurnElements(sse, turnID)
	// The previous turn (if any) is now the latest — repopulate its action bar.
	if prevTurnID != "" {
		c.patchTurnActions(sse, aiTurnActionsView{SessionID: sessionID, TurnID: prevTurnID, CSRF: csrfFromRequest(r)})
	}
}

// handleWorkflowAIRetry handles POST /console/api/workflows/ai/{sessionID}/retry
// — the per-turn "Try again" action. It reverts the draft to the pre-turn
// checkpoint, drops the turn's assistant reply (keeping the user instruction),
// and re-dispatches a fresh run driven by that same instruction, streaming it
// into the same turn (replacing the discarded one). Rejected inline while a run
// is already in flight on the session.
func (c *Console) handleWorkflowAIRetry(w http.ResponseWriter, r *http.Request) {
	if w == nil || r == nil {
		panic("handleWorkflowAIRetry: nil w or r")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIRetry: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.aiBuilder == nil || c.buildStream == nil || c.deps.BuilderSessions == nil {
		c.serveNotFound(w, r)
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	formTurnID := r.FormValue("msg_id")

	sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai retry load session", "err", err)
		c.serveError(w, r)
		return
	}

	// In-flight guard: never dispatch a second run while one is generating.
	if sess.Status == store.BuilderSessionGenerating {
		c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: formTurnID, CSRF: csrfFromRequest(r), Error: "Still generating…"})
		return
	}

	cp, err := c.deps.BuilderSessions.LatestCheckpoint(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: formTurnID, CSRF: csrfFromRequest(r), Error: "Nothing to retry."})
			return
		}
		c.deps.Logger.Error("console: ai retry checkpoint", "err", err)
		c.serveError(w, r)
		return
	}
	turnID := cp.MessageID

	// Recover the user instruction driving the turn (the run's loadState reads it
	// from history too, but the re-streamed user bubble needs the text).
	instruction := c.turnInstructionText(r, scope, sessionID, turnID)

	// Revert the draft to the pre-turn snapshot (non-destructive — the checkpoint
	// stays so the retried turn is itself discardable/retriable), then drop the
	// discarded turn's assistant reply while keeping the user instruction.
	if err := c.deps.BuilderSessions.RevertToCheckpoint(r.Context(), scope, sessionID, cp.ID); err != nil {
		c.deps.Logger.Error("console: ai retry revert", "err", err)
		c.serveError(w, r)
		return
	}
	if err := c.deps.BuilderSessions.DeleteMessagesFrom(r.Context(), scope, sessionID, turnID, false); err != nil {
		c.deps.Logger.Error("console: ai retry delete assistant reply", "err", err)
	}

	// Mark in-flight so a concurrent turn is rejected; the finish step flips it
	// back to Ready.
	if err := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sessionID, store.BuilderSessionGenerating); err != nil {
		c.deps.Logger.Error("console: ai retry set status", "err", err)
	}

	runID, err := c.aiBuilder.Dispatch(r.Context(), scope, sessionID)
	if err != nil {
		c.deps.Logger.Error("console: ai retry dispatch", "err", err)
		c.resetInFlight(scope, sessionID)
		c.serveTurnActions(w, r, aiTurnActionsView{SessionID: sessionID, TurnID: turnID, CSRF: csrfFromRequest(r), Error: "Could not restart the builder."})
		return
	}

	// Re-stream into the SAME turn id: isRetry=true makes streamAICopilotTurn
	// first remove the stale turn elements, then re-append the user bubble + a
	// fresh assistant shell.
	c.streamAICopilotTurn(w, r, scope, sessionID, runID, instruction, turnID, false, true)
}

// turnInstructionText returns the content of the user message identified by
// messageID (the instruction that drove a turn), or "" when it is not found.
func (c *Console) turnInstructionText(r *http.Request, scope domain.Scope, sessionID, messageID string) string {
	msgs, err := c.deps.BuilderSessions.Messages(r.Context(), scope, sessionID)
	if err != nil {
		return ""
	}
	for _, m := range msgs {
		if m.ID == messageID {
			return m.Content
		}
	}
	return ""
}

// serveTurnActions renders the ai-turn-actions fragment as a one-shot SSE patch
// (its root #ai-msg-{TurnID}-actions drives the outer-mode replace). Used by the
// synchronous action responses (Keep result, discard/retry rejections).
func (c *Console) serveTurnActions(w http.ResponseWriter, r *http.Request, v aiTurnActionsView) {
	sse := datastar.NewSSE(w, r)
	c.patchTurnActions(sse, v)
}
