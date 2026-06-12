// workflows_test.go proves the Wave-1 workflows pages render correctly
// off a seeded store: list page with all rows, fragment search+status
// filter, empty states, auth on all new routes, and the read-only
// detail page.
package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
)

// ---- list page ----

func TestWorkflowsList_RendersSeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Welcome", "welcome-email")
	tc.seedWorkflow(t, "Digest", "daily-digest")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Both workflow names must appear.
	for _, want := range []string{"Welcome", "Digest", "welcome-email", "daily-digest"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}
	// Count tile.
	if !strings.Contains(body, `<span class="page-header-tile-count">2</span>`) {
		t.Errorf("list page missing count tile with 2")
	}
	// Status chips present.
	if !strings.Contains(body, `chip-success`) {
		t.Errorf("list page missing active chip")
	}
	// Filter bar present.
	if !strings.Contains(body, `id="wf-search"`) {
		t.Errorf("list page missing search input")
	}
	if !strings.Contains(body, `id="wf-status"`) {
		t.Errorf("list page missing status select")
	}
	// Create workflow CTA present.
	if !strings.Contains(body, "Create workflow") {
		t.Errorf("list page missing Create workflow CTA")
	}
	// Nav: Workflows entry is active (Section == "workflows").
	if !strings.Contains(body, `is-active`) {
		t.Errorf("nav missing is-active on the Workflows entry")
	}
}

func TestWorkflowsList_EmptyStore(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No workflows yet") {
		t.Errorf("empty list page missing empty-state title")
	}
	if !strings.Contains(body, "Create your first workflow") {
		t.Errorf("empty list page missing CTA description")
	}
}

func TestWorkflowsList_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/workflows", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("GET /console/workflows without session: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("Location = %q, want /console/login", got)
	}
}

// ---- fragment filter ----

func TestFragmentWorkflowsList_SearchFilter(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Welcome", "welcome-email")
	tc.seedWorkflow(t, "Digest", "daily-digest")
	cookie, _ := tc.login(t)

	// Signal: wfFilter = "welcome"
	v := url.Values{}
	v.Set("datastar", `{"wfFilter":"welcome","wfStatus":""}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/workflows-list", v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// SSE framing.
	if !strings.Contains(body, "event: datastar-patch-elements") {
		t.Errorf("fragment response missing SSE event framing")
	}
	// Only Welcome should appear; Digest filtered out.
	if !strings.Contains(body, "Welcome") {
		t.Errorf("fragment missing Welcome row")
	}
	if strings.Contains(body, "Digest") {
		t.Errorf("fragment should not contain Digest row (filtered out)")
	}
}

func TestFragmentWorkflowsList_StatusFilter(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Welcome", "welcome-email")
	// Seed an inactive workflow by creating and toggling via store directly.
	tc.seedInactiveWorkflow(t, "Paused", "paused-wf")
	cookie, _ := tc.login(t)

	// Filter to active only.
	v := url.Values{}
	v.Set("datastar", `{"wfFilter":"","wfStatus":"active"}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/workflows-list", v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("active filter: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Welcome") {
		t.Errorf("active filter missing Welcome row")
	}
	if strings.Contains(body, "Paused") {
		t.Errorf("active filter should not contain inactive Paused row")
	}
}

func TestFragmentWorkflowsList_EmptyFilterShowsNoMatchState(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Welcome", "welcome-email")
	cookie, _ := tc.login(t)

	v := url.Values{}
	v.Set("datastar", `{"wfFilter":"zzznomatch","wfStatus":""}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/workflows-list", v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("no-match filter: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No matches") {
		t.Errorf("no-match filter missing empty-state 'No matches'")
	}
}

func TestFragmentWorkflowsList_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/api/fragments/workflows-list", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("fragment without session: status = %d, want 303", rr.Code)
	}
}

// ---- detail page ----

func TestWorkflowDetail_RendersDefinition(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome-email", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"Hello"}`)},
		{StepID: "s2", Type: "email", ControlValues: json.RawMessage(`{"subject":"Hi"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/welcome-email", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows/welcome-email: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Workflow name in header.
	if !strings.Contains(body, "Welcome") {
		t.Errorf("detail page missing workflow name")
	}
	// Trigger identifier displayed.
	if !strings.Contains(body, "welcome-email") {
		t.Errorf("detail page missing trigger identifier")
	}
	// Active chip.
	if !strings.Contains(body, "chip-success") {
		t.Errorf("detail page missing active chip")
	}
	// Step types rendered.
	if !strings.Contains(body, "in_app") {
		t.Errorf("detail page missing in_app step chip")
	}
	if !strings.Contains(body, "email") {
		t.Errorf("detail page missing email step chip")
	}
	// Control values shown.
	if !strings.Contains(body, "Hello") {
		t.Errorf("detail page missing in_app controlValues body")
	}
	// Edit affordance present but disabled.
	if !strings.Contains(body, "Edit workflow") {
		t.Errorf("detail page missing Edit workflow stub")
	}
	if !strings.Contains(body, "Wave 2") {
		t.Errorf("detail page Edit stub missing Wave 2 tooltip")
	}
	// Activity tab stub present.
	if !strings.Contains(body, "Activity") {
		t.Errorf("detail page missing Activity tab stub")
	}
}

func TestWorkflowDetail_NotFoundSlugReturns404(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/nonexistent-slug", cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /console/workflows/nonexistent-slug: status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Page not found") {
		t.Errorf("404 page missing 'Page not found'")
	}
}

func TestWorkflowDetail_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/workflows/some-slug", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("detail without session: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("Location = %q, want /console/login", got)
	}
}

// ---- scope isolation ----

// TestWorkflowsList_ScopeIsolation proves the list page only renders
// workflows for the logged-in session's scope: a workflow seeded under a
// second org/env must not leak into another tenant's list.
func TestWorkflowsList_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	other := tc.altScope(t)
	tc.seedWorkflow(t, "Mine", "mine-wf")
	tc.seedWorkflowInScope(t, other, "Theirs", "theirs-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Mine") || !strings.Contains(body, "mine-wf") {
		t.Errorf("list page missing the in-scope workflow")
	}
	if strings.Contains(body, "Theirs") || strings.Contains(body, "theirs-wf") {
		t.Errorf("list page leaked another scope's workflow")
	}
	// The unfiltered count tile must show 1, not 2.
	if !strings.Contains(body, `<span class="page-header-tile-count">1</span>`) {
		t.Errorf("count tile leaked the other scope's workflow (want 1)")
	}
}

// TestWorkflowDetail_ScopeIsolation proves the detail route is scope-bound:
// a slug that exists only under another scope 404s for this session.
func TestWorkflowDetail_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	other := tc.altScope(t)
	tc.seedWorkflowInScope(t, other, "Theirs", "theirs-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/theirs-wf", cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /console/workflows/theirs-wf (other scope): status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Page not found") {
		t.Errorf("cross-scope detail did not render the 404 page")
	}
}

// ---- security: clipboard JS-string escaping ----

// TestWorkflowDetail_TriggerIdentifierJSEscaped proves a trigger
// identifier containing a single quote round-trips into the clipboard
// button's Datastar expression as a properly escaped JS literal, instead
// of breaking out of the string. The jsEscape template func emits the
// quotes and JS-escapes the contents; html/template then HTML-escapes the
// quotes for the attribute, so the rendered bytes carry `\&#39;` (a
// browser-decoded `\'`) rather than a bare, literal-breaking quote.
func TestWorkflowDetail_TriggerIdentifierJSEscaped(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Quoted", "o'reilly-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/o'reilly-wf", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET detail with quote slug: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// The single quote inside the JS literal must be backslash-escaped
	// (rendered as the HTML entity for a quote, preceded by a backslash).
	if !strings.Contains(body, `writeText(&#39;o\&#39;reilly-wf&#39;)`) {
		t.Errorf("clipboard expression did not JS-escape the quote in the trigger identifier; body excerpt missing escaped literal")
	}
	// The raw, unescaped literal-breaking form must NOT appear.
	if strings.Contains(body, `writeText('o'reilly-wf')`) {
		t.Errorf("clipboard expression contains a raw quote that breaks the JS literal")
	}
}

// TestWorkflowRow_TriggerIdentifierJSEscaped is the list-table counterpart:
// the same escaping must hold in the workflow-row partial the fragment
// endpoint also renders.
func TestWorkflowRow_TriggerIdentifierJSEscaped(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Quoted", "o'reilly-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET list with quote slug: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `writeText(&#39;o\&#39;reilly-wf&#39;)`) {
		t.Errorf("row clipboard expression did not JS-escape the quote in the trigger identifier")
	}
	if strings.Contains(body, `writeText('o'reilly-wf')`) {
		t.Errorf("row clipboard expression contains a raw quote that breaks the JS literal")
	}
}

// ---- unit: buildWorkflowRows ----

func TestBuildWorkflowRows_SearchMatchesNameAndTrigger(t *testing.T) {
	workflows := []domain.Workflow{
		{Name: "Alpha", TriggerIdentifier: "alpha-id", Active: true, Steps: json.RawMessage(`[]`)},
		{Name: "Beta", TriggerIdentifier: "beta-id", Active: true, Steps: json.RawMessage(`[]`)},
	}
	// Match by name.
	rows := buildWorkflowRows(workflows, "alpha", "")
	if len(rows) != 1 || rows[0].Name != "Alpha" {
		t.Errorf("name match: got %d rows (%v), want 1 Alpha", len(rows), rows)
	}
	// Match by trigger identifier.
	rows = buildWorkflowRows(workflows, "beta-id", "")
	if len(rows) != 1 || rows[0].TriggerIdentifier != "beta-id" {
		t.Errorf("trigger match: got %d rows, want 1 beta-id", len(rows))
	}
	// No match.
	rows = buildWorkflowRows(workflows, "zzz", "")
	if len(rows) != 0 {
		t.Errorf("no-match: got %d rows, want 0", len(rows))
	}
}

func TestBuildWorkflowRows_StatusFilter(t *testing.T) {
	workflows := []domain.Workflow{
		{Name: "Active WF", TriggerIdentifier: "active-wf", Active: true, Steps: json.RawMessage(`[]`)},
		{Name: "Inactive WF", TriggerIdentifier: "inactive-wf", Active: false, Steps: json.RawMessage(`[]`)},
	}
	active := buildWorkflowRows(workflows, "", "active")
	if len(active) != 1 || !active[0].Active {
		t.Errorf("active filter: got %d rows, want 1 active", len(active))
	}
	inactive := buildWorkflowRows(workflows, "", "inactive")
	if len(inactive) != 1 || inactive[0].Active {
		t.Errorf("inactive filter: got %d rows, want 1 inactive", len(inactive))
	}
	all := buildWorkflowRows(workflows, "", "")
	if len(all) != 2 {
		t.Errorf("all filter: got %d rows, want 2", len(all))
	}
}

func TestBuildWorkflowRows_StepChipsDecoded(t *testing.T) {
	workflows := []domain.Workflow{
		{
			Name:              "Multi",
			TriggerIdentifier: "multi",
			Active:            true,
			Steps: json.RawMessage(`[
				{"stepId":"s1","type":"in_app","controlValues":{}},
				{"stepId":"s2","type":"email","controlValues":{}}
			]`),
		},
	}
	rows := buildWorkflowRows(workflows, "", "")
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if len(rows[0].Steps) != 2 {
		t.Errorf("want 2 steps, got %d", len(rows[0].Steps))
	}
	if rows[0].Steps[0].Type != "in_app" {
		t.Errorf("step[0].Type = %q, want in_app", rows[0].Steps[0].Type)
	}
}

// ---- unit: prettyJSON ----

func TestPrettyJSON(t *testing.T) {
	cases := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{"nil", nil, ""},
		{"empty", json.RawMessage{}, ""},
		{"invalid", json.RawMessage(`{bad`), ""},
		{"object", json.RawMessage(`{"a":1}`), "{\n  \"a\": 1\n}"},
	}
	for _, tc := range cases {
		got := prettyJSON(tc.input)
		if got != tc.want {
			t.Errorf("prettyJSON(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---- helpers ----

// getWithQuery sends a GET with an explicit query string appended.
func (tc *testConsole) getWithQuery(t *testing.T, path, query string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	full := path
	if query != "" {
		full += "?" + query
	}
	return tc.get(t, full, cookie)
}

// seedInactiveWorkflow creates a workflow and immediately marks it inactive
// in the store, bypassing the authoring service's "active on create" default.
// Uses the same raw-SQL approach as revokeAllKeys (opens the temp db by path).
func (tc *testConsole) seedInactiveWorkflow(t *testing.T, name, trigger string) {
	t.Helper()
	ctx := context.Background()
	wf, err := tc.deps.Authoring.Create(ctx, tc.scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
		Steps:             []domain.Step{{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatalf("seed inactive workflow create: %v", err)
	}
	// Mark inactive via a raw handle on the same dbPath (store does not export one).
	db, err := sql.Open("sqlite", tc.dbPath)
	if err != nil {
		t.Fatalf("seed inactive: open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE workflows SET active = 0 WHERE id = ?`, wf.ID); err != nil {
		t.Fatalf("seed inactive workflow update: %v", err)
	}
}

// seedWorkflowWithSteps creates a workflow with the given steps.
func (tc *testConsole) seedWorkflowWithSteps(t *testing.T, name, trigger string, steps []domain.Step) {
	t.Helper()
	_, err := tc.deps.Authoring.Create(context.Background(), tc.scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("seedWorkflowWithSteps: %v", err)
	}
}
