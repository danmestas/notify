package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// seedWorkflowWithStepsInScope creates a workflow under an explicit scope with
// the given steps, for the refine tenancy test (seedWorkflowWithSteps uses
// tc.scope).
func (tc *testConsole) seedWorkflowWithStepsInScope(t *testing.T, scope domain.Scope, name, trigger string, steps []domain.Step) {
	t.Helper()
	if _, err := tc.deps.Authoring.Create(context.Background(), scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
		Steps:             steps,
	}); err != nil {
		t.Fatalf("seed workflow with steps: %v", err)
	}
}

// seedRefineDraft creates a refine-mode builder session under scope targeting
// slug and holding the refined draft (what the durable run leaves behind),
// returning its id (in the terminal Ready status the durable run leaves behind).
// Apply/discard tests start from this state.
func (tc *testConsole) seedRefineDraft(t *testing.T, scope domain.Scope, slug string, draft aibuilder.WorkflowDraft) string {
	t.Helper()
	return tc.seedRefineDraftStatus(t, scope, slug, draft, store.BuilderSessionReady)
}

// seedRefineDraftStatus is seedRefineDraft with an explicit terminal status, so a
// test can seed a still-running (draft) refine session to prove the mid-run apply
// guard.
func (tc *testConsole) seedRefineDraftStatus(t *testing.T, scope domain.Scope, slug string, draft aibuilder.WorkflowDraft, status string) string {
	t.Helper()
	dj, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal refine draft: %v", err)
	}
	sess, err := tc.st.BuilderSessions().CreateRefine(context.Background(), scope, "seed refine prompt", slug, dj)
	if err != nil {
		t.Fatalf("seed refine session: %v", err)
	}
	if status != store.BuilderSessionDraft {
		if err := tc.st.BuilderSessions().UpdateDraft(context.Background(), scope, sess.ID, dj, status); err != nil {
			t.Fatalf("set refine session status: %v", err)
		}
	}
	return sess.ID
}

// TestAIRefine_SeedsSessionFromWorkflowAndStreamsPreview drives the refine
// endpoint: it seeds a refine session from the existing workflow (mode=refine,
// target slug, draft = the workflow's steps), dispatches, and streams a preview
// carrying the refined proposal with an Apply/Discard action pair pointing at the
// apply endpoint for this slug.
func TestAIRefine_SeedsSessionFromWorkflowAndStreamsPreview(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
		{StepID: "step_delay1", Type: "delay", ControlValues: json.RawMessage(`{"type":"regular","amount":1,"unit":"days"}`)},
	})

	// The refined draft the durable run leaves behind: the original two steps plus
	// a new in-app step.
	refined := &aibuilder.WorkflowDraft{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
			{StepID: "step_delay1", Type: "delay", ControlValues: json.RawMessage(`{"type":"regular","amount":1,"unit":"days"}`)},
			{StepID: "step_inapp1", Type: "in_app", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"Still here?"}`)},
		},
	}
	events := [][]byte{
		aiStreamMsg(t, "reasoning", "Adding an in-app nudge.", refined, ""),
		aiStreamMsg(t, "canvas", "", refined, ""),
		aiStreamMsg(t, "done", "Added an in-app nudge after the delay.", refined, ""),
	}
	tc.injectStreamingBuilder("run-refine", refined, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/welcome/ai-refine", url.Values{
		"prompt":     {"add an in-app nudge after the delay"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The preview renders the refined steps and an Apply action targeting the
	// slug-scoped apply endpoint.
	if !strings.Contains(body, "Apply changes") {
		t.Errorf("preview missing Apply changes; body: %s", body)
	}
	if !strings.Contains(body, "/console/api/workflows/welcome/ai-apply/") {
		t.Errorf("preview Apply action does not target the slug apply endpoint; body: %s", body)
	}
	if !strings.Contains(body, "Nudge") && !strings.Contains(body, "In-App") {
		t.Errorf("preview does not show the refined in-app step; body: %s", body)
	}

	// Exactly one builder session was created, and it is a refine session seeded
	// from the existing workflow targeting its slug.
	sess := onlyBuilderSession(t, tc)
	if sess.Mode != store.BuilderSessionModeRefine {
		t.Fatalf("session mode = %q, want refine", sess.Mode)
	}
	if sess.TargetSlug != "welcome" {
		t.Fatalf("session targetSlug = %q, want welcome", sess.TargetSlug)
	}
}

// TestAIApply_UpdatesWorkflowSteps proves the apply endpoint loads the refined
// draft and pushes it through authoring.Update: the workflow gains the new step
// and the response redirects to the workflow detail page.
func TestAIApply_UpdatesWorkflowSteps(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
	})

	// A refine session seeded from the workflow, holding the refined draft (the
	// original email step plus a new in-app step) — what the durable run leaves.
	sessID := tc.seedRefineDraft(t, tc.scope, "welcome", aibuilder.WorkflowDraft{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
			{StepID: "step_inapp1", Type: "in_app", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"Still here?"}`)},
		},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/welcome/ai-apply/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("apply status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/console/workflows/welcome" {
		t.Fatalf("redirect Location = %q, want /console/workflows/welcome", loc)
	}

	// The workflow now has the refined steps.
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome")
	if err != nil {
		t.Fatalf("get updated workflow: %v", err)
	}
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		t.Fatalf("decode updated steps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("updated workflow steps = %d, want 2", len(steps))
	}
	if steps[1].Type != "in_app" || steps[1].StepID != "step_inapp1" {
		t.Fatalf("second step = %+v, want the new in-app step", steps[1])
	}

	// The session is marked confirmed (applied).
	got, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != store.BuilderSessionConfirmed {
		t.Errorf("session status = %q, want confirmed", got.Status)
	}
}

// TestAIApply_DiscardLeavesWorkflowUnchanged proves discarding a refine session
// does not touch the underlying workflow (the refined draft is dropped).
func TestAIApply_DiscardLeavesWorkflowUnchanged(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
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

	rr := tc.postForm(t, "/console/api/workflows/ai-discard/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("discard status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	// The workflow is unchanged: still exactly one email step.
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome")
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		t.Fatalf("decode steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Type != "email" {
		t.Fatalf("workflow changed after discard: %+v", steps)
	}
	got, _ := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if got.Status != store.BuilderSessionDiscarded {
		t.Errorf("session status = %q, want discarded", got.Status)
	}
}

// TestAIApply_CrossWorkflowTargetMismatch404s proves a refine session that targets
// workflow A cannot be applied onto workflow B: POSTing A's session id to B's
// apply endpoint 404s and leaves BOTH workflows unchanged. Without the
// TargetSlug==slug guard, A's refined draft would clobber B.
func TestAIApply_CrossWorkflowTargetMismatch404s(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	// Workflow A (the refine target) and workflow B (the victim), each one step.
	tc.seedWorkflowWithSteps(t, "Workflow A", "workflow-a", []domain.Step{
		{StepID: "step_a1", Type: "email", Name: "A", ControlValues: json.RawMessage(`{"subject":"A","body":"A"}`)},
	})
	tc.seedWorkflowWithSteps(t, "Workflow B", "workflow-b", []domain.Step{
		{StepID: "step_b1", Type: "in_app", Name: "B", ControlValues: json.RawMessage(`{"body":"B"}`)},
	})
	// A refine session for A holding a two-step refined draft.
	sessID := tc.seedRefineDraft(t, tc.scope, "workflow-a", aibuilder.WorkflowDraft{
		Name:              "Workflow A",
		TriggerIdentifier: "workflow-a",
		Steps: []domain.Step{
			{StepID: "step_a1", Type: "email", Name: "A", ControlValues: json.RawMessage(`{"subject":"A","body":"A"}`)},
			{StepID: "step_a2", Type: "in_app", Name: "Extra", ControlValues: json.RawMessage(`{"body":"extra"}`)},
		},
	})
	cookie, csrf := tc.login(t)

	// Apply A's session against B's URL — must 404.
	rr := tc.postForm(t, "/console/api/workflows/workflow-b/ai-apply/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-workflow apply status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}

	// Workflow B is untouched (still its single in-app step, not A's draft).
	wfB, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "workflow-b")
	if err != nil {
		t.Fatalf("get workflow-b: %v", err)
	}
	var stepsB []domain.Step
	if err := json.Unmarshal(wfB.Steps, &stepsB); err != nil {
		t.Fatalf("decode workflow-b steps: %v", err)
	}
	if len(stepsB) != 1 || stepsB[0].StepID != "step_b1" {
		t.Fatalf("workflow-b was clobbered by A's draft: %+v", stepsB)
	}
	// Workflow A is also untouched (apply did not run at all).
	wfA, _ := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "workflow-a")
	var stepsA []domain.Step
	_ = json.Unmarshal(wfA.Steps, &stepsA)
	if len(stepsA) != 1 {
		t.Fatalf("workflow-a changed on a mismatched apply: %+v", stepsA)
	}
}

// TestAIApply_MidRunDraftRejected proves a refine session that is still running
// (status draft — the initial/mid-run state, not the terminal Ready) cannot be
// applied: the handler rejects it inline and the workflow is unchanged. Without
// the Ready guard, the original (un-refined) draft would be applied to the live
// workflow.
func TestAIApply_MidRunDraftRejected(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
	})
	// A refine session still in the mid-run 'draft' status (not Ready).
	sessID := tc.seedRefineDraftStatus(t, tc.scope, "welcome", aibuilder.WorkflowDraft{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
			{StepID: "step_inapp1", Type: "in_app", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"Still here?"}`)},
		},
	}, store.BuilderSessionDraft)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/welcome/ai-apply/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	// Rejected: not a redirect (no apply happened).
	if rr.Code == http.StatusSeeOther {
		t.Fatalf("mid-run apply must not redirect (it applied the draft); status = %d", rr.Code)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("mid-run apply status = %d, want 200 with inline rejection (body: %s)", rr.Code, rr.Body.String())
	}

	// The workflow is unchanged: still exactly one email step.
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome")
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		t.Fatalf("decode steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Type != "email" {
		t.Fatalf("workflow mutated by a mid-run apply: %+v", steps)
	}
	// The session was not advanced to confirmed.
	got, _ := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if got.Status == store.BuilderSessionConfirmed {
		t.Fatal("mid-run apply marked the session confirmed")
	}
}

// TestAIRefine_ScopeIsolated proves a refine on a workflow belonging to another
// environment 404s — no cross-tenant seed and no session leaks into the caller's
// scope.
func TestAIRefine_ScopeIsolated(t *testing.T) {
	tc := newTestConsole(t)
	alt := tc.altScope(t)
	tc.seedWorkflowWithStepsInScope(t, alt, "Secret", "secret", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	// Wire a builder so the nil-gate is not what returns; the 404 must come from
	// the cross-scope workflow lookup.
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/secret/ai-refine", url.Values{
		"prompt":     {"leak this"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-scope refine status = %d, want 404", rr.Code)
	}
	if len(builderSessionIDs(t, tc)) != 0 {
		t.Fatal("cross-scope refine created a session in the caller's scope")
	}
}

// TestAIRefine_Unconfigured_Graceful asserts the refine endpoint returns a clear
// not-configured message (never a panic / 500) when no builder is wired.
func TestAIRefine_Unconfigured_Graceful(t *testing.T) {
	tc := newTestConsole(t) // no AIBuilder injected -> aiBuilder nil
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/welcome/ai-refine", url.Values{
		"prompt":     {"anything"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not configured") {
		t.Errorf("expected a not-configured message; body: %s", rr.Body.String())
	}
}

// TestWorkflowDetail_RefineSidebar asserts the workflow detail page renders the
// Refine-with-AI sidebar (prompt + stream regions) when the builder is wired, and
// the graceful not-configured note when it is not.
func TestWorkflowDetail_RefineSidebar(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	cookie, _ := tc.login(t)

	// Unconfigured: a not-configured note, no refine form.
	rr := tc.get(t, "/console/workflows/welcome", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not configured") {
		t.Errorf("expected the refine not-configured note when unwired")
	}

	// Configured: the refine form + stream regions render.
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	rr = tc.get(t, "/console/workflows/welcome", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("configured status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"ai-refine",
		`id="ai-builder-reasoning"`,
		`id="ai-builder-canvas"`,
		`id="ai-builder-preview"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("configured detail page missing %q; body did not contain it", want)
		}
	}
}
