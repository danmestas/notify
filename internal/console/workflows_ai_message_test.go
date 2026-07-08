package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// TestAIMessage_ContinuesSessionCheckpointsAndAccumulates drives the copilot
// /message endpoint against an existing (turn-1) session and asserts the full
// Slice-1 plumbing: the user turn is appended to the chat history, a checkpoint of
// the pre-turn draft is pushed (linked to that message), the run is dispatched, and
// the streamed result — a draft that accumulates the turn-1 email step plus a new
// sms step — is relayed as the final preview and persisted on the session.
func TestAIMessage_ContinuesSessionCheckpointsAndAccumulates(t *testing.T) {
	tc := newTestConsole(t)

	// Turn 1 already ran: the session holds a one-step (email) Ready draft.
	turn1 := aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)},
		},
	}
	sessID := tc.seedBuilderDraft(t, tc.scope, turn1)

	// Turn 2's run accumulates: email (preserved) + a new sms step. The stub
	// dispatcher persists this to the session (mirroring the durable finish step),
	// and the stream carries it as the terminal done event.
	turn2 := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)},
			{StepID: "s2", Type: "sms", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"Don't forget us"}`)},
		},
	}
	events := [][]byte{
		aiStreamMsg(t, "reasoning", "Adding an sms reminder.", turn2, ""),
		aiStreamMsg(t, "canvas", "", turn2, ""),
		aiStreamMsg(t, "done", "Added an sms reminder.", turn2, ""),
	}
	tc.injectStreamingBuilder("run-msg", turn2, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt":     {"add an sms reminder after the email"},
		"view":       {"copilot"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// The accumulated workflow is relayed into the docked canvas (not a full-screen
	// preview), and the turn's action bar becomes available.
	if !strings.Contains(body, `data-component="ai-builder-canvas"`) {
		t.Errorf("accumulated canvas not relayed; body: %s", body)
	}
	if !strings.Contains(body, "ai-msg-actions") {
		t.Errorf("turn action bar not rendered on the copilot turn; body: %s", body)
	}

	ctx := context.Background()

	// The user turn was appended to the session's chat history (the copilot turn
	// also persists an assistant summary after it, so the user turn is msgs[0]).
	msgs, err := tc.st.BuilderSessions().Messages(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) == 0 || msgs[0].Role != "user" || msgs[0].Content != "add an sms reminder after the email" {
		t.Fatalf("chat history = %+v, want the user turn with the instruction first", msgs)
	}

	// A checkpoint of the PRE-turn (turn-1) draft was pushed, linked to that
	// message — the undo point for this turn.
	cp, err := tc.st.BuilderSessions().LatestCheckpoint(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	if cp.MessageID != msgs[0].ID {
		t.Errorf("checkpoint messageID = %q, want the appended message id %q", cp.MessageID, msgs[0].ID)
	}
	var snap aibuilder.WorkflowDraft
	if err := json.Unmarshal(cp.DraftJSON, &snap); err != nil {
		t.Fatalf("decode checkpoint draft: %v", err)
	}
	if len(snap.Steps) != 1 || snap.Steps[0].Type != "email" {
		t.Fatalf("checkpoint snapshot = %+v, want the pre-turn one-email-step draft", snap.Steps)
	}

	// The session now holds the accumulated (2-step) draft: the turn built on top of
	// turn 1 rather than restarting.
	sess, err := tc.st.BuilderSessions().Get(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	var final aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &final); err != nil {
		t.Fatalf("decode final draft: %v", err)
	}
	if len(final.Steps) != 2 || final.Steps[0].Type != "email" || final.Steps[1].Type != "sms" {
		t.Fatalf("final draft = %+v, want the accumulated [email, sms]", final.Steps)
	}

	// Reverting the checkpoint restores the pre-turn draft (round-trip), proving the
	// undo point is usable by a later slice's Discard handler.
	if err := tc.st.BuilderSessions().RevertToCheckpoint(ctx, tc.scope, sessID, cp.ID); err != nil {
		t.Fatalf("RevertToCheckpoint: %v", err)
	}
	reverted, err := tc.st.BuilderSessions().Get(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("Get after revert: %v", err)
	}
	var back aibuilder.WorkflowDraft
	if err := json.Unmarshal(reverted.DraftWorkflow, &back); err != nil {
		t.Fatalf("decode reverted draft: %v", err)
	}
	if len(back.Steps) != 1 || back.Steps[0].Type != "email" {
		t.Fatalf("reverted draft = %+v, want the pre-turn one-email-step draft", back.Steps)
	}
}

// TestAIMessage_UnknownSessionNotFound proves a message to a missing (or
// cross-tenant) session is a 404 — no session leaks, no checkpoint, no dispatch.
func TestAIMessage_UnknownSessionNotFound(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/does-not-exist/message", url.Values{
		"prompt":     {"do something"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestAIMessage_EmptyPromptInlineError proves an empty message resolves inline
// (HTTP 200 with guidance) rather than dispatching a run or 500-ing.
func TestAIMessage_EmptyPromptInlineError(t *testing.T) {
	tc := newTestConsole(t)
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt":     {"   "},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "describe the change") {
		t.Errorf("expected an inline guidance message; body: %s", rr.Body.String())
	}
	// No checkpoint pushed for a rejected empty message.
	if _, err := tc.st.BuilderSessions().LatestCheckpoint(context.Background(), tc.scope, sessID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestCheckpoint err = %v, want ErrNotFound (no checkpoint for a rejected message)", err)
	}
}
