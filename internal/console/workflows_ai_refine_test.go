package console

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
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
// slug and holding the refined draft in the terminal Ready status the durable run
// leaves behind. Keep tests start from this state.
func (tc *testConsole) seedRefineDraft(t *testing.T, scope domain.Scope, slug string, draft aibuilder.WorkflowDraft) string {
	t.Helper()
	dj, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal refine draft: %v", err)
	}
	sess, err := tc.st.BuilderSessions().CreateRefine(context.Background(), scope, "seed refine prompt", slug, dj)
	if err != nil {
		t.Fatalf("seed refine session: %v", err)
	}
	if err := tc.st.BuilderSessions().UpdateDraft(context.Background(), scope, sess.ID, dj, store.BuilderSessionReady); err != nil {
		t.Fatalf("set refine session status: %v", err)
	}
	return sess.ID
}

// TestAIBuilderView_SlugSeedsRefineSession proves GET /console/workflows/ai?slug=
// seeds a fresh refine-mode session from the existing workflow: the canvas renders
// its current steps, the panel shows refine starter suggestions with an empty
// thread, and the bound session id routes the operator's first message to the
// session's /message endpoint (Keep → authoring.Update).
func TestAIBuilderView_SlugSeedsRefineSession(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
		{StepID: "step_delay1", Type: "delay", ControlValues: json.RawMessage(`{"type":"regular","amount":1,"unit":"days"}`)},
	})
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/ai?slug=welcome", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := html.UnescapeString(rr.Body.String())

	// The canvas is seeded with the workflow's current steps + name.
	for _, want := range []string{
		`data-component="ai-builder-canvas"`, // seeded canvas rendered
		"Welcome",                            // workflow name in the canvas
		"Email",                              // first step label
		"Delay",                              // second step label
		`"aiStarted":false`,                  // empty thread — starters show
		"Add an SMS reminder",                // a refine starter suggestion
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refine builder view missing %q; body: %s", want, body)
		}
	}
	// Create-mode starters must NOT appear (this is refine mode).
	if strings.Contains(body, "Welcome series") {
		t.Errorf("refine view leaked create-mode suggestions; body: %s", body)
	}

	// Exactly one builder session was created: a refine session targeting welcome,
	// bound to the panel so follow-ups route to it.
	sess := onlyBuilderSession(t, tc)
	if sess.Mode != store.BuilderSessionModeRefine {
		t.Fatalf("session mode = %q, want refine", sess.Mode)
	}
	if sess.TargetSlug != "welcome" {
		t.Fatalf("session targetSlug = %q, want welcome", sess.TargetSlug)
	}
	if !strings.Contains(body, `"aiSessionID":"`+sess.ID+`"`) {
		t.Errorf("refine session id not bound to the panel; body: %s", body)
	}

	// The seeded session holds the workflow's current draft (2 steps) so the first
	// turn accumulates on it rather than starting from scratch.
	var seeded struct {
		Steps []domain.Step `json:"steps"`
	}
	if err := json.Unmarshal(sess.DraftWorkflow, &seeded); err != nil {
		t.Fatalf("decode seeded draft: %v", err)
	}
	if len(seeded.Steps) != 2 {
		t.Fatalf("seeded draft steps = %d, want 2 (the workflow's current shape)", len(seeded.Steps))
	}
}

// TestAIBuilderView_SlugCrossScope404 proves ?slug for a workflow in another
// environment 404s — no cross-tenant seed and no session leaks into the caller's
// scope.
func TestAIBuilderView_SlugCrossScope404(t *testing.T) {
	tc := newTestConsole(t)
	alt := tc.altScope(t)
	tc.seedWorkflowWithStepsInScope(t, alt, "Secret", "secret", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/ai?slug=secret", cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-scope slug status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
	if len(builderSessionIDs(t, tc)) != 0 {
		t.Fatal("cross-scope refine created a session in the caller's scope")
	}
}

// TestAIBuilderView_NoSlugCreatePathEmpty proves the create path (no ?slug, no
// ?session) is unchanged: no session is created, the canvas is empty, and the
// create-mode starter suggestions render.
func TestAIBuilderView_NoSlugCreatePathEmpty(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/ai", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := html.UnescapeString(rr.Body.String())

	if strings.Contains(body, `data-component="ai-builder-canvas"`) {
		t.Errorf("create path rendered a seeded canvas; body: %s", body)
	}
	if !strings.Contains(body, `"aiSessionID":""`) {
		t.Errorf("create path bound a session id; body: %s", body)
	}
	if !strings.Contains(body, "Welcome series") {
		t.Errorf("create path missing create-mode suggestions; body: %s", body)
	}
	if len(builderSessionIDs(t, tc)) != 0 {
		t.Errorf("create path created a builder session before the first turn")
	}
}

// TestWorkflowDetail_RefineWithAIButton asserts the workflow detail page renders a
// "Refine with AI" link to the ?slug builder view when the builder is wired, and
// omits it entirely when the builder is not configured.
func TestWorkflowDetail_RefineWithAIButton(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	cookie, _ := tc.login(t)

	// Unconfigured: no refine link.
	rr := tc.get(t, "/console/workflows/welcome", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "/console/workflows/ai?slug=welcome") {
		t.Errorf("unconfigured detail page must not render the Refine with AI link")
	}

	// Configured: the "Refine with AI" link to the ?slug builder view renders.
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	rr = tc.get(t, "/console/workflows/welcome", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("configured status = %d, want 200", rr.Code)
	}
	body := html.UnescapeString(rr.Body.String())
	if !strings.Contains(body, "Refine with AI") {
		t.Errorf("configured detail page missing the Refine with AI heading/link; body: %s", body)
	}
	if !strings.Contains(body, "/console/workflows/ai?slug=welcome") {
		t.Errorf("Refine with AI link does not point at the ?slug builder view; body: %s", body)
	}
}
