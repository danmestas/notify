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

// smtpForEmail seeds an active SMTP integration so the email step type is
// available to authoring.Create/Update (mirrors the confirm/apply tests).
func (tc *testConsole) smtpForEmail(t *testing.T) {
	t.Helper()
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
}

// TestAIKeep_CreateSessionPersistsWorkflow proves Keep on a create session runs
// the draft through authoring.Create: the workflow exists afterwards, the session
// is confirmed, and the action bar flips to the Saved · Open workflow state.
func TestAIKeep_CreateSessionPersistsWorkflow(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/keep", url.Values{
		"csrf_token": {csrf},
		"msg_id":     {"turn-1"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("keep status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Saved") {
		t.Errorf("keep response missing Saved state; body: %s", body)
	}
	if !strings.Contains(body, `href="/console/workflows/welcome-flow"`) {
		t.Errorf("keep response missing Open-workflow link; body: %s", body)
	}

	// The workflow now exists (this is the load-bearing assertion: Keep == Create).
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome-flow")
	if err != nil {
		t.Fatalf("kept workflow not found: %v", err)
	}
	if wf.Name != "Welcome Flow" {
		t.Errorf("kept workflow name = %q, want Welcome Flow", wf.Name)
	}

	got, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != store.BuilderSessionConfirmed {
		t.Errorf("session status = %q, want confirmed", got.Status)
	}
}

// TestAIKeep_RefineSessionUpdatesWorkflow proves Keep on a refine session runs the
// draft through authoring.Update against the session's target slug: the workflow
// gains the refined step, and the session is confirmed.
func TestAIKeep_RefineSessionUpdatesWorkflow(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
	})
	sessID := tc.seedRefineDraft(t, tc.scope, "welcome", aibuilder.WorkflowDraft{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
			{StepID: "step_inapp1", Type: "in_app", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"Still here?"}`)},
		},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/keep", url.Values{
		"csrf_token": {csrf},
		"msg_id":     {"turn-1"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("keep status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `href="/console/workflows/welcome"`) {
		t.Errorf("keep response missing Open-workflow link to the target; body: %s", body)
	}

	// Keep == Update: the workflow gains the refined in-app step.
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome")
	if err != nil {
		t.Fatalf("get updated workflow: %v", err)
	}
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		t.Fatalf("decode updated steps: %v", err)
	}
	if len(steps) != 2 || steps[1].Type != "in_app" {
		t.Fatalf("updated steps = %+v, want [email, in_app]", steps)
	}

	got, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != store.BuilderSessionConfirmed {
		t.Errorf("session status = %q, want confirmed", got.Status)
	}
}

// TestAIKeep_RejectsWhileGenerating proves Keep on an in-flight session is a no-op
// with an inline note — no workflow is created.
func TestAIKeep_RejectsWhileGenerating(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	if err := tc.st.BuilderSessions().SetStatus(context.Background(), tc.scope, sessID, store.BuilderSessionGenerating); err != nil {
		t.Fatalf("set generating: %v", err)
	}
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/keep", url.Values{
		"csrf_token": {csrf}, "msg_id": {"turn-1"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("keep status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "generating") {
		t.Errorf("expected an inline still-generating note; body: %s", rr.Body.String())
	}
	if _, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome-flow"); err == nil {
		t.Error("workflow was created despite the in-flight guard")
	}
}

// runOneCopilotTurn drives a single /message turn on sessID that accumulates the
// prior draft into next (persisted by the stub as the run's finish). It returns
// after the turn is done (checkpoint pushed, draft accumulated).
func (tc *testConsole) runOneCopilotTurn(t *testing.T, sessID, runID, prompt string, next *aibuilder.WorkflowDraft, cookie *http.Cookie, csrf string) {
	t.Helper()
	events := [][]byte{
		aiStreamMsg(t, "canvas", "", next, ""),
		aiStreamMsg(t, "done", "Updated the workflow.", next, ""),
	}
	tc.injectStreamingBuilder(runID, next, false, events)
	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt": {prompt}, "view": {"copilot"}, "csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("message turn status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestAIDiscardTurn_RevertsDraftAndLeavesNoWorkflow proves Discard reverts the
// draft to the pre-turn checkpoint, drops the turn, and never creates a workflow.
func TestAIDiscardTurn_RevertsDraftAndLeavesNoWorkflow(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)

	// Turn 1: a one-step (email) draft.
	turn1 := aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)}},
	}
	sessID := tc.seedBuilderDraft(t, tc.scope, turn1)
	cookie, csrf := tc.login(t)

	// Turn 2 accumulates a second (sms) step — /message pushes a pre-turn checkpoint.
	turn2 := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)},
			{StepID: "s2", Type: "sms", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"psst"}`)},
		},
	}
	tc.runOneCopilotTurn(t, sessID, "run-2", "add an sms reminder", turn2, cookie, csrf)

	ctx := context.Background()
	// Sanity: turn 2 is present (draft has 2 steps, a checkpoint exists).
	if _, err := tc.st.BuilderSessions().LatestCheckpoint(ctx, tc.scope, sessID); err != nil {
		t.Fatalf("expected a checkpoint after the turn: %v", err)
	}

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/discard-turn", url.Values{
		"csrf_token": {csrf}, "msg_id": {"ignored"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("discard status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	// The re-rendered canvas shows the reverted (one-step) draft.
	if body := rr.Body.String(); !strings.Contains(body, `data-component="ai-builder-canvas"`) {
		t.Errorf("discard did not re-render the canvas; body: %s", body)
	}

	// The draft reverted to the pre-turn (one-step) snapshot.
	sess, err := tc.st.BuilderSessions().Get(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	var reverted aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &reverted); err != nil {
		t.Fatalf("decode reverted draft: %v", err)
	}
	if len(reverted.Steps) != 1 || reverted.Steps[0].Type != "email" {
		t.Fatalf("reverted draft = %+v, want the one-step email draft", reverted.Steps)
	}
	if sess.Status != store.BuilderSessionReady {
		t.Errorf("session status = %q, want ready after discard", sess.Status)
	}
	// The checkpoint was popped and the turn's messages truncated.
	if _, err := tc.st.BuilderSessions().LatestCheckpoint(ctx, tc.scope, sessID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("checkpoint not popped: err = %v, want ErrNotFound", err)
	}
	if msgs, _ := tc.st.BuilderSessions().Messages(ctx, tc.scope, sessID); len(msgs) != 0 {
		t.Errorf("turn messages not truncated: %+v", msgs)
	}
	// No workflow was ever created by a discard.
	if _, err := tc.deps.Authoring.GetBySlug(ctx, tc.scope, "welcome-flow"); err == nil {
		t.Error("discard created a workflow; it must not")
	}
}

// TestAIDiscardTurn_OnlyTurn_ResetsStartedSignal is the L8 regression: when
// Discard removes the SESSION'S ONLY turn (leaving zero messages, the same
// condition serveWorkflowAIBuilder's resume path uses to decide $aiStarted -
// see aiBuilderSignalsJSON's len(view.Messages) > 0 rule), the copilot panel
// must revert to the friendly starter-cards empty state rather than showing a
// bare, message-less thread. The handler must patch $aiStarted back to false.
func TestAIDiscardTurn_OnlyTurn_ResetsStartedSignal(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)
	cookie, csrf := tc.login(t)

	draft := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)}},
	}
	tc.injectStreamingBuilder("run-1", draft, false, [][]byte{
		aiStreamMsg(t, "canvas", "", draft, ""),
		aiStreamMsg(t, "done", "Built the workflow.", draft, ""),
	})
	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt": {"build a welcome flow"}, "view": {"copilot"}, "csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	sessID := onlyBuilderSession(t, tc).ID

	rr2 := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/discard-turn", url.Values{
		"csrf_token": {csrf}, "msg_id": {"ignored"},
	}, cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("discard status = %d, want 200 (body: %s)", rr2.Code, rr2.Body.String())
	}
	body := rr2.Body.String()
	if !strings.Contains(body, "datastar-patch-signals") {
		t.Errorf("discarding the only turn must patch signals to restore the empty state; body: %s", body)
	}
	if !strings.Contains(body, `"aiStarted":false`) {
		t.Errorf("discarding the only turn must reset $aiStarted so the starter-cards empty state shows again; body: %s", body)
	}

	msgs, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no messages after discarding the only turn, got %d", len(msgs))
	}
}

// TestAIDiscardTurn_MultiTurn_KeepsStartedSignal proves the companion case:
// discarding a LATER turn that leaves an earlier turn's message behind must
// NOT reset $aiStarted — the thread still has content to show.
func TestAIDiscardTurn_MultiTurn_KeepsStartedSignal(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)
	cookie, csrf := tc.login(t)

	draft1 := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)}},
	}
	tc.injectStreamingBuilder("run-1", draft1, false, [][]byte{
		aiStreamMsg(t, "canvas", "", draft1, ""),
		aiStreamMsg(t, "done", "Built the workflow.", draft1, ""),
	})
	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt": {"build a welcome flow"}, "view": {"copilot"}, "csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	sessID := onlyBuilderSession(t, tc).ID

	draft2 := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)},
			{StepID: "s2", Type: "sms", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"psst"}`)},
		},
	}
	tc.runOneCopilotTurn(t, sessID, "run-2", "add an sms reminder", draft2, cookie, csrf)

	rr2 := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/discard-turn", url.Values{
		"csrf_token": {csrf}, "msg_id": {"ignored"},
	}, cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("discard status = %d, want 200 (body: %s)", rr2.Code, rr2.Body.String())
	}
	if body := rr2.Body.String(); strings.Contains(body, `"aiStarted":false`) {
		t.Errorf("discarding a later turn with a remaining prior turn must not reset $aiStarted; body: %s", body)
	}
}

// TestAIRetry_RevertsThenRedispatches proves Try-again reverts to the pre-turn
// checkpoint and re-dispatches the same instruction, rebuilding the draft as a
// fresh turn (the checkpoint survives; the assistant reply is replaced).
func TestAIRetry_RevertsThenRedispatches(t *testing.T) {
	tc := newTestConsole(t)
	tc.smtpForEmail(t)

	turn1 := aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)}},
	}
	sessID := tc.seedBuilderDraft(t, tc.scope, turn1)
	cookie, csrf := tc.login(t)

	// Turn 2 (original): email + sms.
	turn2 := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)},
			{StepID: "s2", Type: "sms", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"psst"}`)},
		},
	}
	tc.runOneCopilotTurn(t, sessID, "run-2", "add a reminder", turn2, cookie, csrf)

	ctx := context.Background()
	cpBefore, err := tc.st.BuilderSessions().LatestCheckpoint(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("checkpoint before retry: %v", err)
	}

	// The retry re-dispatch rebuilds the turn differently: email + in_app.
	retryDraft := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{}`)},
			{StepID: "s3", Type: "in_app", Name: "Ping", ControlValues: json.RawMessage(`{"body":"hey"}`)},
		},
	}
	events := [][]byte{
		aiStreamMsg(t, "canvas", "", retryDraft, ""),
		aiStreamMsg(t, "done", "Rebuilt the turn.", retryDraft, ""),
	}
	tc.injectStreamingBuilder("run-retry", retryDraft, false, events)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/retry", url.Values{
		"csrf_token": {csrf}, "msg_id": {"ignored"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	// A fresh turn was streamed (the same user instruction bubble + a new canvas).
	if body := rr.Body.String(); !strings.Contains(body, "add a reminder") ||
		!strings.Contains(body, `data-component="ai-builder-canvas"`) {
		t.Errorf("retry did not re-stream a fresh turn; body: %s", body)
	}
	// Retry re-streams into the SAME turn id, so it must first drop that turn's
	// stale DOM (the prior attempt's chips/reasoning/actions) before re-appending —
	// unlike a brand-new turn (see TestAIBuilderCopilot_CreateDoesNotEmitStaleRemovalNoise).
	if body := rr.Body.String(); !strings.Contains(body, "mode remove") {
		t.Errorf("retry did not clear the stale turn's DOM before re-streaming; body: %s", body)
	}

	// The draft was rebuilt to the retry's shape (email + in_app), not turn 2's sms.
	sess, err := tc.st.BuilderSessions().Get(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	var rebuilt aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &rebuilt); err != nil {
		t.Fatalf("decode rebuilt draft: %v", err)
	}
	if len(rebuilt.Steps) != 2 || rebuilt.Steps[1].Type != "in_app" {
		t.Fatalf("rebuilt draft = %+v, want [email, in_app]", rebuilt.Steps)
	}
	if sess.Status != store.BuilderSessionReady {
		t.Errorf("session status = %q, want ready after retry finish", sess.Status)
	}

	// The pre-turn checkpoint survived the retry (the retried turn is itself undoable).
	cpAfter, err := tc.st.BuilderSessions().LatestCheckpoint(ctx, tc.scope, sessID)
	if err != nil {
		t.Fatalf("checkpoint after retry: %v", err)
	}
	if cpAfter.ID != cpBefore.ID {
		t.Errorf("checkpoint changed across retry: %q -> %q (should be non-destructive)", cpBefore.ID, cpAfter.ID)
	}

	// Exactly one user + one (regenerated) assistant message remain — no duplicate.
	msgs, _ := tc.st.BuilderSessions().Messages(ctx, tc.scope, sessID)
	var users, assistants int
	for _, m := range msgs {
		switch m.Role {
		case "user":
			users++
		case "assistant":
			assistants++
		}
	}
	if users != 1 || assistants != 1 {
		t.Errorf("message counts = %d user / %d assistant, want 1/1; msgs: %+v", users, assistants, msgs)
	}
}

// TestAIMessage_InFlightGuardRejectsConcurrent proves a /message dispatched while
// a run is already generating on the session is rejected inline — no second run,
// no checkpoint, no appended turn.
func TestAIMessage_InFlightGuardRejectsConcurrent(t *testing.T) {
	tc := newTestConsole(t)
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	// A run is already in flight on this session.
	if err := tc.st.BuilderSessions().SetStatus(context.Background(), tc.scope, sessID, store.BuilderSessionGenerating); err != nil {
		t.Fatalf("set generating: %v", err)
	}
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt": {"add another step"}, "view": {"copilot"}, "csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "generating") {
		t.Errorf("expected an inline still-generating note; body: %s", rr.Body.String())
	}

	ctx := context.Background()
	// The guard fired before any mutation: no checkpoint, no appended message.
	if _, err := tc.st.BuilderSessions().LatestCheckpoint(ctx, tc.scope, sessID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a checkpoint was pushed despite the guard: err = %v", err)
	}
	if msgs, _ := tc.st.BuilderSessions().Messages(ctx, tc.scope, sessID); len(msgs) != 0 {
		t.Errorf("a message was appended despite the guard: %+v", msgs)
	}
}

// TestAIRetry_RejectsWhileGenerating proves Try-again is rejected inline while a
// run is already in flight on the session (no revert, no re-dispatch).
func TestAIRetry_RejectsWhileGenerating(t *testing.T) {
	tc := newTestConsole(t)
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name: "Welcome Flow", TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	if err := tc.st.BuilderSessions().SetStatus(context.Background(), tc.scope, sessID, store.BuilderSessionGenerating); err != nil {
		t.Fatalf("set generating: %v", err)
	}
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/retry", url.Values{
		"csrf_token": {csrf}, "msg_id": {"turn-1"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "generating") {
		t.Errorf("expected an inline still-generating note; body: %s", rr.Body.String())
	}
}
