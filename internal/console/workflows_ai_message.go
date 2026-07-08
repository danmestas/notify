package console

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// handleWorkflowAIMessage handles POST /console/api/workflows/ai/{sessionID}/message.
// It continues an existing builder session as a multi-turn copilot chat: it loads
// the scope-checked session, snapshots the current draft as a per-turn checkpoint
// (so the turn can be reverted later), records the user's new instruction on the
// session's message history, marks the session in-progress, and dispatches a fresh
// durable build run. The run's generalized loadState seeds from the current draft
// and is driven by the just-appended instruction, so the workflow accumulates
// across turns rather than restarting. Live events stream into a fresh assistant
// turn in the docked #ai-thread. A missing or cross-scope session 404s; an
// unconfigured builder returns a graceful message. Works for both create and
// refine sessions — the per-turn Keep handler routes on the session's mode.
//
// The per-turn Keep / Discard / Try-again handlers (which consume the checkpoints
// pushed here) land in a later slice; this endpoint only produces them.
func (c *Console) handleWorkflowAIMessage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowAIMessage: w is nil")
	}
	if r == nil {
		panic("handleWorkflowAIMessage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIMessage: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	if c.aiBuilder == nil || c.buildStream == nil || c.deps.BuilderSessions == nil {
		c.patchAIError(w, r, "AI builder not configured. Set NOTIFY_AI_API_KEY (or ANTHROPIC_API_KEY) to enable the copilot.")
		return
	}

	// Load the session under scope first: a missing or cross-tenant id 404s. The
	// session's mode/target are consulted at Keep time (create → Create, refine →
	// Update against TargetSlug); the turn itself streams identically for either.
	sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai message load session", "err", err)
		c.serveError(w, r)
		return
	}

	// In-flight guard: a session already generating must not dispatch a second run
	// (the two would race on one draft). Reject inline; the client shows the note
	// and keeps the composer's text.
	if sess.Status == store.BuilderSessionGenerating {
		c.patchAIError(w, r, "Still generating… wait for the current turn to finish.")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		c.patchAIError(w, r, "Describe the change you want before sending.")
		return
	}
	if len([]rune(prompt)) > aiPromptMaxLen {
		c.patchAIError(w, r, "Message is too long — please keep it under 2 000 characters.")
		return
	}
	if !c.aiLimiter.Allow(scope) {
		c.patchAIError(w, r, "You're generating a lot right now — wait a moment and try again.")
		return
	}

	// Record the user turn on the session's chat history. Its id links the
	// checkpoint to the message that triggered it, and the generalized loadState
	// reads this message as the turn's driving instruction.
	msgID, err := c.deps.BuilderSessions.AppendMessage(r.Context(), scope, sessionID, "user", prompt, "")
	if err != nil {
		c.deps.Logger.Error("console: ai message append", "err", err)
		c.patchAIError(w, r, "Could not send your message. Please try again.")
		return
	}

	// Snapshot the current draft BEFORE the turn runs so the turn can be reverted.
	if _, err := c.deps.BuilderSessions.PushCheckpoint(r.Context(), scope, sessionID, msgID, sess.DraftWorkflow); err != nil {
		c.deps.Logger.Error("console: ai message push checkpoint", "err", err)
		c.patchAIError(w, r, "Could not send your message. Please try again.")
		return
	}

	// Mark the session Generating for the new run: this is the in-flight marker the
	// guard above (and /retry) reject a concurrent turn on, AND it keeps the
	// stream's readiness poll from reading the prior turn's Ready status as "this
	// run already finished". The durable finish step flips it back to Ready; a
	// failure resets it off Generating (streamAICopilotTurn.resetInFlight).
	if err := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sessionID, store.BuilderSessionGenerating); err != nil {
		c.deps.Logger.Error("console: ai message set status", "err", err)
	}

	runID, err := c.aiBuilder.Dispatch(r.Context(), scope, sessionID)
	if err != nil {
		c.deps.Logger.Error("console: ai message dispatch run", "err", err)
		c.resetInFlight(scope, sessionID)
		c.patchAIError(w, r, "Could not start the builder. Please try again.")
		return
	}

	// Stream this follow-up into a NEW assistant turn appended to #ai-thread, keyed
	// on the just-appended user message id so multi-turn appends never collide.
	c.streamAICopilotTurn(w, r, scope, sessionID, runID, prompt, msgID, false)
}
