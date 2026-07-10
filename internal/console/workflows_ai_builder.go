package console

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// aiBuilderPageView binds the /console/workflows/ai two-pane builder page: the
// live canvas (left) plus the docked copilot chat panel (right). Signals seeds
// the Datastar signals for the panel's collapse/resize + send routing. Messages
// replays a resumed session's prior turns into the thread.
type aiBuilderPageView struct {
	AIConfigured bool
	Suggestions  []string
	SessionID    string
	Signals      template.JS
	Canvas       aiBuilderCanvasView
	Messages     []aiThreadMessageView
	CSRF         string
}

// aiThreadMessageView is one replayed (or freshly-appended) turn in #ai-thread.
// A user turn renders a right-aligned bubble from Text; an assistant turn renders
// the per-message chip/reasoning/status/actions containers keyed by ID. Working
// shows the "Working" spinner on a still-streaming assistant turn; Chips seeds any
// already-known chips (empty during live streaming — chips arrive as SSE patches).
// Actions, when set, seeds the per-turn Keep/Discard/Try-again bar on the LATEST
// assistant turn of a resumed session (empty during live streaming — the bar is
// patched in on the done event).
type aiThreadMessageView struct {
	ID      string
	Role    string
	Text    string
	Working bool
	Chips   []aiChipView
	Actions *aiTurnActionsView
}

// aiTurnActionsView binds the ai-turn-actions fragment: the per-turn action bar.
// SessionID + TurnID + CSRF wire the Keep/Discard/Try-again posts. In the Saved
// state (rendered after Keep) Slug links to the persisted workflow; Error carries
// an inline rejection (e.g. a concurrent turn still generating).
type aiTurnActionsView struct {
	SessionID string
	TurnID    string
	CSRF      string
	Saved     bool
	Slug      string
	Error     string
}

// aiChipView binds one chain-of-thought chip: its stable per-tool-call id, human
// label, and streaming|done|error state.
type aiChipView struct {
	ToolCallID string
	Label      string
	State      string
}

// copilotTurnState threads the mutable per-stream cursor through relayAICopilotEvent
// so successive tool events land in the right assistant turn and the previous chip
// flips streaming→done when the next one appears.
type copilotTurnState struct {
	TurnID        string
	LastChipID    string
	LastChipLabel string
}

// aiBuilderSignalsJSON seeds the copilot page's Datastar signals: panel collapse
// + width, the drag flag, the active session id (empty until the first create
// stream patches it), the bound composer text, and whether the thread has started
// (true when replaying an existing session so the empty state stays hidden).
func aiBuilderSignalsJSON(sessionID string, started bool) template.JS {
	b, err := json.Marshal(map[string]any{
		"aiCollapsed": false,
		"aiWidth":     420,
		"aiResizing":  false,
		"aiSessionID": sessionID,
		"aiPrompt":    "",
		"aiStarted":   started,
	})
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// serveWorkflowAIBuilder renders GET /console/workflows/ai — the docked-copilot
// workflow builder. An optional ?session={id} resumes an existing session: its
// draft seeds the canvas and its stored turns replay into the thread. The panel
// is gated on AIConfigured (an unwired builder shows a not-configured note).
func (c *Console) serveWorkflowAIBuilder(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowAIBuilder: w is nil")
	}
	if r == nil {
		panic("serveWorkflowAIBuilder: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowAIBuilder: no scope in context")
	}

	view := aiBuilderPageView{
		AIConfigured: c.aiBuilder != nil,
		Suggestions:  aiBuilderCreateSuggestions,
	}

	// The composer's datastar @post serializes the form (incl. the hidden
	// csrf_token) — wire the per-session token so the mutation passes CSRF. Read
	// it before replay so the resumed latest turn's action bar can carry it too.
	if s, ok := sessionFromContext(r.Context()); ok {
		view.CSRF = s.CSRF
	}

	// Resume an existing session when ?session= is present and reachable in scope.
	if sessionID := r.URL.Query().Get("session"); sessionID != "" && c.deps.BuilderSessions != nil {
		if sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID); err == nil {
			view.SessionID = sess.ID
			if len(sess.DraftWorkflow) > 0 {
				var draft aibuilder.WorkflowDraft
				if uerr := json.Unmarshal(sess.DraftWorkflow, &draft); uerr == nil {
					view.Canvas = c.aiCanvasFromDraft(&draft)
				}
			}
			view.Messages = c.aiThreadReplay(r.Context(), scope, sess, view.CSRF)
		} else if !errors.Is(err, store.ErrNotFound) {
			c.deps.Logger.Error("console: ai builder resume session", "err", err)
		}
	} else if slug := r.URL.Query().Get("slug"); slug != "" {
		// Refine an existing workflow: seed a FRESH refine-mode session from its
		// current shape (name + trigger identifier + steps), render the canvas
		// seeded with those steps, and switch the panel to refine mode (refine
		// starters, empty thread). The user drives changes via the session's
		// /message endpoint; Keep routes to authoring.Update against the target
		// slug. A missing/cross-scope slug 404s so no session leaks into scope.
		wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
		if err != nil {
			if isNotFound(err) {
				c.serveNotFound(w, r)
				return
			}
			c.deps.Logger.Error("console: ai builder refine load workflow", "slug", slug, "err", err)
			c.serveError(w, r)
			return
		}
		seed, err := draftFromWorkflow(wf)
		if err != nil {
			c.deps.Logger.Error("console: ai builder refine decode workflow steps", "slug", slug, "err", err)
			c.serveError(w, r)
			return
		}
		view.Suggestions = aiBuilderRefineSuggestions
		view.Canvas = c.aiCanvasFromDraft(&seed)
		if c.deps.BuilderSessions != nil {
			if seedJSON, merr := json.Marshal(seed); merr == nil {
				if sess, cerr := c.deps.BuilderSessions.CreateRefine(r.Context(), scope, "", slug, seedJSON); cerr == nil {
					view.SessionID = sess.ID
				} else {
					c.deps.Logger.Error("console: ai builder create refine session", "slug", slug, "err", cerr)
				}
			}
		}
	}

	view.Signals = aiBuilderSignalsJSON(view.SessionID, len(view.Messages) > 0)
	c.renderPage(w, r, http.StatusOK, "workflows-ai", "AI workflow builder", view)
}

// aiThreadReplay projects a session's stored message history into thread turns.
// Only user turns and assistant summary turns are persisted (chips/reasoning are
// streamed, not stored), so a replayed assistant turn shows its summary line.
//
// Turns are keyed exactly like the live stream so per-turn patches (discard's DOM
// removal, the action bar) hit the same ids after a reload: a user bubble takes
// id ai-turn-{userMsgID}-u, and the assistant turn it precedes takes id
// ai-turn-{userMsgID} (the USER message id, which /message + the action bar both
// address). The LATEST assistant turn of a Ready session gets its Keep/Discard/
// Try-again bar seeded so the actions survive a resume.
func (c *Console) aiThreadReplay(ctx context.Context, scope domain.Scope, sess store.BuilderSession, csrf string) []aiThreadMessageView {
	if c.deps.BuilderSessions == nil {
		return nil
	}
	msgs, err := c.deps.BuilderSessions.Messages(ctx, scope, sess.ID)
	if err != nil {
		c.deps.Logger.Error("console: ai builder replay messages", "err", err)
		return nil
	}
	out := make([]aiThreadMessageView, 0, len(msgs))
	lastUserID := ""       // the user msg id the current turn's assistant is keyed by
	lastAssistantIdx := -1 // index in out of the most recent assistant turn
	for _, m := range msgs {
		if m.Role == "assistant" {
			turnID := m.ID
			if lastUserID != "" {
				turnID = lastUserID
			}
			out = append(out, aiThreadMessageView{ID: turnID, Role: "assistant", Text: m.Content})
			lastAssistantIdx = len(out) - 1
			lastUserID = ""
			continue
		}
		lastUserID = m.ID
		out = append(out, aiThreadMessageView{ID: m.ID + "-u", Role: "user", Text: m.Content})
	}
	// Seed the action bar on the latest assistant turn of a finished (Ready)
	// session so a resumed thread can Keep/Discard/Try-again without waiting for a
	// fresh turn. A still-generating or already-terminal session shows no bar.
	if lastAssistantIdx >= 0 && sess.Status == store.BuilderSessionReady {
		out[lastAssistantIdx].Actions = &aiTurnActionsView{
			SessionID: sess.ID,
			TurnID:    out[lastAssistantIdx].ID,
			CSRF:      csrf,
		}
	}
	return out
}

// streamAICopilotTurn is the docked-panel counterpart of streamAIBuild: it opens
// the SSE stream, appends the user bubble and a fresh assistant turn shell to
// #ai-thread, optionally patches $aiSessionID (first/create turn), then relays the
// run's events into that turn's per-message containers until a terminal event, the
// deadline, or a failed run resolves it. userMsgID keys the assistant turn so
// multi-turn appends never collide. isRetry marks a Try-again re-stream into the
// SAME turn id (see removeTurnElements below) — a create or follow-up turn is
// always a fresh id and must NOT set it.
func (c *Console) streamAICopilotTurn(w http.ResponseWriter, r *http.Request, scope domain.Scope, sessionID, runID, userText, userMsgID string, emitSessionID, isRetry bool) {
	timeout := aiGenerateTimeout
	if c.aiGenerateTimeoutOverride > 0 {
		timeout = c.aiGenerateTimeoutOverride
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	events := make(chan []byte, liveTailBuffer)
	unsub, err := c.buildStream.SubscribeBuildStream(runID, func(d []byte) {
		cp := make([]byte, len(d))
		copy(cp, d)
		select {
		case events <- cp:
		default:
		}
	})
	if err != nil {
		c.deps.Logger.Error("console: ai copilot subscribe build stream", "err", err)
		c.patchAIError(w, r, "Could not start the builder. Please try again.")
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

	// Only a Try-again re-streams into the SAME turn id — drop its stale DOM
	// first so the fresh appends below don't collide with the retried turn's old
	// elements. A create/follow-up turn's id has never existed in the DOM, so
	// this removal is gated on isRetry rather than fired unconditionally: an
	// unconditional remove-mode patch against a selector that matches nothing
	// is exactly what trips the Datastar client's PatchElementsNoTargetsFound
	// console warning, and it would otherwise fire on every single turn.
	if isRetry {
		c.removeTurnElements(sse, userMsgID)
	}

	// Render this turn's shells into the thread before the first event arrives,
	// then scroll the newly-sent turn into view — a long prior thread would
	// otherwise leave it (and the "Working" status) below the fold.
	c.appendThreadMessage(sse, aiThreadMessageView{ID: userMsgID + "-u", Role: "user", Text: userText})
	c.appendThreadMessage(sse, aiThreadMessageView{ID: userMsgID, Role: "assistant", Working: true})
	c.scrollThreadToBottom(sse)
	if emitSessionID {
		if perr := sse.MarshalAndPatchSignals(map[string]string{"aiSessionID": sessionID}); perr != nil {
			c.deps.Logger.Debug("console: ai copilot patch session signal", "err", perr)
		}
	}
	// Clear the composer now that the turn is definitely accepted (dispatched
	// and streaming) — never on the client, and never on a pre-turn rejection
	// (patchAIError, which never reaches this function), so a rejected send
	// (rate limit, over-long, still generating) leaves the typed text in place
	// for the user to edit and resend (M8). A Try-again re-stream reuses the
	// stored instruction rather than the composer, so it does not touch it.
	if !isRetry {
		if perr := sse.MarshalAndPatchSignals(map[string]string{"aiPrompt": ""}); perr != nil {
			c.deps.Logger.Debug("console: ai copilot clear prompt signal", "err", perr)
		}
	}

	turn := &copilotTurnState{TurnID: userMsgID}
	for {
		select {
		case <-ctx.Done():
			lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			sess, sessErr := c.deps.BuilderSessions.Get(lookupCtx, scope, sessionID)
			lookupCancel()
			if sessErr == nil && sess.Status == store.BuilderSessionReady {
				c.emitAICanvas(sse, c.copilotFinalCanvas(r, scope, sessionID, nil))
				c.finalizeCopilotTurn(sse, r, scope, sessionID, turn.TurnID, "")
				return
			}
			c.appendTurnReasoning(sse, turn.TurnID, "Generation did not finish — please try again.")
			c.clearTurnStatus(sse, turn.TurnID)
			c.resetInFlight(scope, sessionID)
			return
		case raw, ok := <-events:
			if !ok {
				return
			}
			if done := c.relayAICopilotEvent(sse, r, scope, sessionID, turn, raw); done {
				return
			}
		case <-ticker.C:
			if failed, ferr := c.aiBuilder.RunFailed(ctx, runID); ferr == nil && failed {
				c.appendTurnReasoning(sse, turn.TurnID, "Generation did not finish — please try again.")
				if turn.LastChipID != "" {
					c.patchChip(sse, turn.LastChipID, turn.LastChipLabel, "error")
				}
				c.clearTurnStatus(sse, turn.TurnID)
				c.resetInFlight(scope, sessionID)
				return
			}
		}
	}
}

// relayAICopilotEvent maps one streamed build Event to the docked-panel patches,
// keyed per-message so a multi-turn thread never collides. reasoning → append a
// line into the turn's reasoning; tool → append a streaming chip (flipping the
// prior chip to done) and re-render the canvas; canvas → re-render the canvas;
// done → finalize the turn (chip done, canvas kept, empty actions placeholder);
// error → chip error + inline note. Returns true when terminal.
func (c *Console) relayAICopilotEvent(sse *datastar.ServerSentEventGenerator, r *http.Request, scope domain.Scope, sessionID string, turn *copilotTurnState, raw []byte) bool {
	var ev aiStreamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		c.deps.Logger.Debug("console: ai copilot stream decode", "err", err)
		return false
	}
	switch aibuilder.EventKind(ev.Kind) {
	case aibuilder.KindReasoning:
		if ev.Text != "" {
			c.appendTurnReasoning(sse, turn.TurnID, ev.Text)
		}
		return false
	case aibuilder.KindTool:
		// Flip the previous chip streaming→done now that the next call arrived.
		if turn.LastChipID != "" && turn.LastChipID != ev.ToolCallID {
			c.patchChip(sse, turn.LastChipID, turn.LastChipLabel, "done")
		}
		label := c.aiChipLabel(ev.ToolName, ev.Draft)
		chipID := ev.ToolCallID
		if chipID == "" {
			chipID = turn.TurnID + "-chip"
		}
		c.appendTurnChip(sse, turn.TurnID, aiChipView{ToolCallID: chipID, Label: label, State: "streaming"})
		turn.LastChipID = chipID
		turn.LastChipLabel = label
		if ev.Draft != nil {
			c.emitAICanvas(sse, c.aiCanvasFromDraft(ev.Draft))
		}
		return false
	case aibuilder.KindCanvas:
		if ev.Draft != nil {
			c.emitAICanvas(sse, c.aiCanvasFromDraft(ev.Draft))
		}
		return false
	case aibuilder.KindDone:
		if turn.LastChipID != "" {
			c.patchChip(sse, turn.LastChipID, turn.LastChipLabel, "done")
		}
		c.emitAICanvas(sse, c.copilotFinalCanvas(r, scope, sessionID, ev.Draft))
		c.finalizeCopilotTurn(sse, r, scope, sessionID, turn.TurnID, ev.Text)
		return true
	case aibuilder.KindError:
		c.deps.Logger.Error("console: ai copilot build error event", "err", ev.Error)
		if turn.LastChipID != "" {
			c.patchChip(sse, turn.LastChipID, turn.LastChipLabel, "error")
		}
		c.appendTurnReasoning(sse, turn.TurnID, "The AI provider returned an error. Please try again.")
		c.clearTurnStatus(sse, turn.TurnID)
		c.resetInFlight(scope, sessionID)
		return true
	default:
		return false
	}
}

// copilotFinalCanvas builds the completion canvas from the session's persisted
// draft (the durable finish step's UpdateDraft is the source of truth), falling
// back to the done event's draft snapshot. Unlike streamAIBuild the copilot keeps
// the canvas visible on done rather than clearing it for a separate preview.
func (c *Console) copilotFinalCanvas(r *http.Request, scope domain.Scope, sessionID string, fallback *aibuilder.WorkflowDraft) aiBuilderCanvasView {
	var draft aibuilder.WorkflowDraft
	if sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID); err == nil && len(sess.DraftWorkflow) > 0 {
		if uerr := json.Unmarshal(sess.DraftWorkflow, &draft); uerr != nil {
			c.deps.Logger.Debug("console: ai copilot final draft decode", "err", uerr)
		}
	}
	if len(draft.Steps) == 0 && fallback != nil {
		draft = *fallback
	}
	return c.aiCanvasFromDraft(&draft)
}

// finalizeCopilotTurn closes out an assistant turn: clears the "Working" spinner,
// renders the per-turn Keep/Discard/Try-again bar into this (now latest) turn,
// supersedes the previous turn's bar (only the latest turn is actionable), and
// persists an assistant summary message so the turn replays on reload. The
// session is Ready here (the durable finish step persisted the draft), so the bar
// is immediately actionable.
func (c *Console) finalizeCopilotTurn(sse *datastar.ServerSentEventGenerator, r *http.Request, scope domain.Scope, sessionID, turnID, summary string) {
	c.clearTurnStatus(sse, turnID)
	// The turn about to become latest supersedes the previous one's bar first, so
	// exactly one action bar is live at a time.
	if prev := c.previousUserTurnID(r.Context(), scope, sessionID, turnID); prev != "" {
		c.supersedeTurnActions(sse, prev)
	}
	c.patchTurnActions(sse, aiTurnActionsView{
		SessionID: sessionID,
		TurnID:    turnID,
		CSRF:      csrfFromRequest(r),
	})
	// The turn's chips/reasoning grew the thread while streaming, so the
	// Keep/Discard/Try-again bar just patched in above may now sit past the
	// fold at short viewports — bring it into view.
	c.scrollThreadToBottom(sse)
	if summary != "" && c.deps.BuilderSessions != nil {
		if _, err := c.deps.BuilderSessions.AppendMessage(r.Context(), scope, sessionID, "assistant", summary, ""); err != nil {
			c.deps.Logger.Debug("console: ai copilot persist assistant turn", "err", err)
		}
	}
}

// patchTurnActions renders the ai-turn-actions fragment and patches it in place
// (its root #ai-msg-{TurnID}-actions drives the default outer-mode patch).
func (c *Console) patchTurnActions(sse *datastar.ServerSentEventGenerator, v aiTurnActionsView) {
	html, err := renderFragment(c.ts.base, "ai-turn-actions", v)
	if err != nil {
		c.deps.Logger.Error("console: render ai turn actions", "err", err)
		return
	}
	if err := sse.PatchElements(html); err != nil {
		c.deps.Logger.Debug("console: ai turn actions patch", "err", err)
	}
}

// supersedeTurnActions empties a turn's action bar (an older turn is no longer
// actionable once a newer one exists), leaving the container in place for a
// later discard to repopulate.
func (c *Console) supersedeTurnActions(sse *datastar.ServerSentEventGenerator, turnID string) {
	if err := sse.PatchElements(`<div id="ai-msg-` + turnID + `-actions" class="ai-msg-actions"></div>`); err != nil {
		c.deps.Logger.Debug("console: ai turn actions supersede", "err", err)
	}
}

// previousUserTurnID returns the user-message id of the turn immediately before
// the one keyed by turnID (turnID is itself a user-message id), or "" when this
// is the first turn. Used to supersede the prior turn's action bar.
func (c *Console) previousUserTurnID(ctx context.Context, scope domain.Scope, sessionID, turnID string) string {
	if c.deps.BuilderSessions == nil {
		return ""
	}
	msgs, err := c.deps.BuilderSessions.Messages(ctx, scope, sessionID)
	if err != nil {
		return ""
	}
	prev := ""
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if m.ID == turnID {
			return prev
		}
		prev = m.ID
	}
	return prev
}

// resetInFlight clears the Generating in-flight marker on a turn that ended
// without success (a provider error, a failed run, or the deadline), so the
// session is not stuck rejecting future turns. It restores Ready when the
// session still holds a usable draft (a failed follow-up leaves the prior turn's
// Ready draft intact), else Draft. Runs on a fresh context — the request's may be
// cancelled on the deadline path.
func (c *Console) resetInFlight(scope domain.Scope, sessionID string) {
	if c.deps.BuilderSessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := store.BuilderSessionDraft
	if sess, err := c.deps.BuilderSessions.Get(ctx, scope, sessionID); err == nil {
		var d aibuilder.WorkflowDraft
		if json.Unmarshal(sess.DraftWorkflow, &d) == nil &&
			(d.Name != "" || d.TriggerIdentifier != "" || len(d.Steps) > 0) {
			status = store.BuilderSessionReady
		}
	}
	if err := c.deps.BuilderSessions.SetStatus(ctx, scope, sessionID, status); err != nil {
		c.deps.Logger.Debug("console: ai reset in-flight status", "err", err)
	}
}

// clearTurnStatus removes the turn's "Working" spinner by replacing its status
// container with an empty one.
func (c *Console) clearTurnStatus(sse *datastar.ServerSentEventGenerator, turnID string) {
	if err := sse.PatchElements(`<div id="ai-msg-` + turnID + `-status" class="ai-msg-status"></div>`); err != nil {
		c.deps.Logger.Debug("console: ai copilot status clear", "err", err)
	}
}

// scrollThreadToBottom scrolls #ai-thread to its newest content. The panel is
// viewport-clamped (see .ai-copilot-layout in app.css) with #ai-thread as its
// only internal scroll region, so the composer itself never moves — but the
// newest turn (and, once a turn finishes, its Keep/Discard/Try-again row) can
// still start below the thread's own visible area on a long conversation or a
// short viewport. Executed client-side via a one-shot <script> patch rather
// than a CSS-only anchor, since the target only exists after the SSE append
// that triggers this call.
func (c *Console) scrollThreadToBottom(sse *datastar.ServerSentEventGenerator) {
	const script = `var t=document.getElementById('ai-thread');if(t){t.scrollTop=t.scrollHeight;}`
	if err := sse.ExecuteScript(script); err != nil {
		c.deps.Logger.Debug("console: ai thread autoscroll", "err", err)
	}
}

// removeTurnElements removes both DOM elements of a turn (the assistant block
// #ai-turn-{id} and the user bubble #ai-turn-{id}-u) via Datastar remove-mode
// patches. Used by discard (drop the reverted turn) and by the retry re-stream
// (drop the stale turn before re-appending). A missing selector is a client-side
// warning, not an error.
func (c *Console) removeTurnElements(sse *datastar.ServerSentEventGenerator, turnID string) {
	for _, sel := range []string{"#ai-turn-" + turnID, "#ai-turn-" + turnID + "-u"} {
		_ = sse.PatchElements(
			"",
			datastar.WithSelector(sel),
			datastar.WithMode(datastar.ElementPatchModeRemove),
		)
	}
}

// csrfFromRequest returns the per-session CSRF token for datastar @post actions,
// or "" when no session is on the request context.
func csrfFromRequest(r *http.Request) string {
	if s, ok := sessionFromContext(r.Context()); ok {
		return s.CSRF
	}
	return ""
}

// appendThreadMessage renders one turn and appends it to #ai-thread.
func (c *Console) appendThreadMessage(sse *datastar.ServerSentEventGenerator, v aiThreadMessageView) {
	html, err := renderFragment(c.ts.base, "ai-message", v)
	if err != nil {
		c.deps.Logger.Error("console: render ai-message fragment", "err", err)
		return
	}
	if err := sse.PatchElements(
		html,
		datastar.WithSelector("#ai-thread"),
		datastar.WithMode(datastar.ElementPatchModeAppend),
	); err != nil {
		c.deps.Logger.Debug("console: ai thread append", "err", err)
	}
}

// appendTurnReasoning appends one reasoning line into a turn's reasoning region.
func (c *Console) appendTurnReasoning(sse *datastar.ServerSentEventGenerator, turnID, text string) {
	html, err := renderFragment(c.ts.base, "ai-builder-reasoning-line", text)
	if err != nil {
		c.deps.Logger.Error("console: render ai reasoning line", "err", err)
		return
	}
	if err := sse.PatchElements(
		html,
		datastar.WithSelector("#ai-msg-"+turnID+"-reasoning"),
		datastar.WithMode(datastar.ElementPatchModeAppend),
	); err != nil {
		c.deps.Logger.Debug("console: ai turn reasoning append", "err", err)
	}
}

// appendTurnChip appends a new chip into a turn's chips region.
func (c *Console) appendTurnChip(sse *datastar.ServerSentEventGenerator, turnID string, chip aiChipView) {
	html, err := renderFragment(c.ts.base, "ai-chip", chip)
	if err != nil {
		c.deps.Logger.Error("console: render ai-chip fragment", "err", err)
		return
	}
	if err := sse.PatchElements(
		html,
		datastar.WithSelector("#ai-msg-"+turnID+"-chips"),
		datastar.WithMode(datastar.ElementPatchModeAppend),
	); err != nil {
		c.deps.Logger.Debug("console: ai turn chip append", "err", err)
	}
}

// patchChip re-renders a chip in place (outer mode matches by its chip-{id})
// to transition its state (streaming→done, or →error).
func (c *Console) patchChip(sse *datastar.ServerSentEventGenerator, chipID, label, state string) {
	html, err := renderFragment(c.ts.base, "ai-chip", aiChipView{ToolCallID: chipID, Label: label, State: state})
	if err != nil {
		c.deps.Logger.Error("console: render ai-chip fragment", "err", err)
		return
	}
	if err := sse.PatchElements(html); err != nil {
		c.deps.Logger.Debug("console: ai chip patch", "err", err)
	}
}

// aiChipLabel maps a builder tool name (+ the post-mutation draft) to a short,
// human chip label. addStep is enriched with the added step's display type
// ("Added step: Email") when the draft is available.
func (c *Console) aiChipLabel(toolName string, draft *aibuilder.WorkflowDraft) string {
	switch toolName {
	case "setWorkflowMetadata":
		if draft != nil && draft.Name != "" {
			return "Named workflow: " + draft.Name
		}
		return "Named workflow"
	case "addStep":
		if draft != nil && len(draft.Steps) > 0 {
			return "Added step: " + c.aiStepLabel(draft.Steps[len(draft.Steps)-1].Type)
		}
		return "Added a step"
	case "addStepInBetween":
		return "Inserted a step"
	case "editStepContent":
		return "Configured a step"
	case "updateStepConditions":
		return "Set step conditions"
	case "removeStep":
		return "Removed a step"
	case "moveStep":
		return "Reordered a step"
	case "updatePayloadSchema":
		return "Updated payload schema"
	case "":
		return "Working"
	default:
		return toolName
	}
}

// aiStepLabel resolves a step type to its registry display name, falling back to
// the raw type token.
func (c *Console) aiStepLabel(typ string) string {
	if def := c.stepTypes.Get(typ); def != nil {
		return def.DisplayName()
	}
	return typ
}
