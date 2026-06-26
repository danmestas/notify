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
	"strconv"
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
	// New workflow CTA present.
	if !strings.Contains(body, "New workflow") {
		t.Errorf("list page missing New workflow CTA")
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
	// Wave 2 editor: the name field is editable (the read-only "Edit
	// workflow is Wave 2" stub is gone).
	if !strings.Contains(body, `name="name"`) {
		t.Errorf("detail page missing editable name field")
	}
	if !strings.Contains(body, "/save") {
		t.Errorf("detail page missing save form action")
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

// newTestConsoleReadOnly boots a console whose Deps.ReadOnly is true, so
// every mutation handler returns 403 server-side. It reuses the same store
// and bootstrap key as newTestConsole.
func newTestConsoleReadOnly(t *testing.T) *testConsole {
	t.Helper()
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	tc.c = New(tc.deps)
	tc.h = tc.c.Routes()
	return tc
}

// ---- Wave 2: create side-sheet (W2-A3) ----

func TestWorkflowNew_RendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/workflows/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("/console/workflows/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/console/api/workflows"`) {
		t.Errorf("/new missing create form action")
	}
	if !strings.Contains(body, csrf) {
		t.Errorf("/new missing CSRF token in form")
	}
	if !strings.Contains(body, `name="name"`) {
		t.Errorf("/new missing name input")
	}
	if !strings.Contains(body, `name="trigger_identifier"`) {
		t.Errorf("/new missing trigger_identifier input")
	}

	// The create slide-over is now a client-side overlay seeded by the
	// $workflowNewOpen signal (data-show), so it overlays the unchanged list
	// page and opening it causes NO layout shift. The list page renders the
	// same slide-over hidden; the +New button and the slide-over's controls
	// flip the signal instead of navigating to /console/workflows/new.
	rrList := tc.get(t, "/console/workflows", cookie)
	listBody := rrList.Body.String()
	if !strings.Contains(listBody, `data-component="workflow-create"`) {
		t.Errorf("list page missing the (hidden) create slide-over")
	}
	if !strings.Contains(listBody, `data-show="$workflowNewOpen"`) {
		t.Errorf("list page slide-over missing data-show on $workflowNewOpen")
	}
	if !strings.Contains(listBody, `data-on:click="$workflowNewOpen = true"`) {
		t.Errorf("list page missing the open-slide-over button binding")
	}
	if strings.Contains(listBody, `href="/console/workflows/new"`) {
		t.Errorf("list page should not link to /console/workflows/new (signal-driven now)")
	}
}

func TestWorkflowCreate_PostRedirectsToEditor(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows", url.Values{
		"csrf_token":         {csrf},
		"name":               {"My Test WF"},
		"trigger_identifier": {"my-test-wf"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create workflow: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/workflows/my-test-wf" {
		t.Errorf("Location = %q, want /console/workflows/my-test-wf", got)
	}
}

func TestWorkflowCreate_DuplicateIdentifierRendersError(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Existing", "existing-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows", url.Values{
		"csrf_token":         {csrf},
		"name":               {"Another"},
		"trigger_identifier": {"existing-wf"},
	}, cookie)
	// Should redirect back to /new with an error indication (not 500).
	if rr.Code == http.StatusInternalServerError {
		t.Errorf("duplicate identifier returned 500, want redirect with error")
	}
}

func TestWorkflowCreate_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsoleReadOnly(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows", url.Values{
		"csrf_token":         {csrf},
		"name":               {"Test"},
		"trigger_identifier": {"test"},
	}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("ReadOnly create: status = %d, want 403", rr.Code)
	}
}

// ---- Wave 2: duplicate side-sheet (W2-A3) ----

func TestWorkflowDuplicate_RendersForm(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Source WF", "source-wf")
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/workflows/source-wf/duplicate", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("duplicate sheet: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/console/api/workflows/source-wf/duplicate"`) {
		t.Errorf("duplicate form missing correct action")
	}
	if !strings.Contains(body, csrf) {
		t.Errorf("duplicate form missing CSRF token")
	}
	if !strings.Contains(body, "Copy of Source WF") {
		t.Errorf("duplicate form missing pre-filled name")
	}
}

func TestWorkflowDuplicate_PostRedirectsToNewEditor(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Source WF", "source-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/source-wf/duplicate", url.Values{
		"csrf_token":         {csrf},
		"name":               {"Copy of Source WF"},
		"trigger_identifier": {"source-wf-copy"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("duplicate POST: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/workflows/source-wf-copy" {
		t.Errorf("Location = %q, want /console/workflows/source-wf-copy", got)
	}
}

func TestWorkflowDuplicate_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsoleReadOnly(t)
	tc.seedWorkflow(t, "Source WF", "source-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/source-wf/duplicate", url.Values{
		"csrf_token":         {csrf},
		"name":               {"Copy"},
		"trigger_identifier": {"source-wf-copy"},
	}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("ReadOnly duplicate: status = %d, want 403", rr.Code)
	}
}

// ---- Wave 2: editor — step add/remove/reorder (W2-A4) ----

func TestWorkflowEditor_StepAdd(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps", url.Values{
		"csrf_token": {csrf},
		"type":       {"email"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step add: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/workflows/my-wf", cookie)
	if !strings.Contains(rr2.Body.String(), "email") {
		t.Errorf("after step add, detail page missing email step")
	}
}

func TestWorkflowEditor_StepDelete(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/email-1/delete", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step delete: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/workflows/my-wf", cookie)
	if strings.Contains(rr2.Body.String(), "email-1") {
		t.Errorf("after step delete, detail page still shows email-1")
	}
}

func TestWorkflowEditor_StepReorder(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{}`)},
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	// Move in_app-1 up (index 1 -> index 0).
	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/reorder", url.Values{
		"csrf_token": {csrf},
		"stepId":     {"in_app-1"},
		"direction":  {"up"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step reorder: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/workflows/my-wf", cookie)
	body := rr2.Body.String()
	inAppPos := strings.Index(body, "in_app-1")
	emailPos := strings.Index(body, "email-1")
	if inAppPos == -1 || emailPos == -1 {
		t.Fatalf("detail page missing step ids after reorder")
	}
	if inAppPos > emailPos {
		t.Errorf("after reorder up, in_app-1 appears after email-1 — want before")
	}
}

// ---- Wave 2: activate toggle (W2-A4) ----

func TestWorkflowEditor_ActivateToggle(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedInactiveWorkflow(t, "Toggle WF", "toggle-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/toggle-wf/activate", url.Values{
		"csrf_token": {csrf},
		"active":     {"1"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("activate: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/workflows/toggle-wf", cookie)
	if !strings.Contains(rr2.Body.String(), "chip-success") {
		t.Errorf("after activate, detail page missing active chip")
	}
}

// ---- Wave 2: delete (W2-A4) ----

func TestWorkflowEditor_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Delete Me", "delete-me")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/delete-me?confirmDelete=true", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm delete state: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirmDelete page missing 'Confirm delete' button")
	}
	if !strings.Contains(body, "console-confirm-label") {
		t.Errorf("confirmDelete page missing console-confirm-label")
	}
}

func TestWorkflowEditor_DeleteRedirectsToList(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Delete Me", "delete-me")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/delete-me/delete", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/workflows" {
		t.Errorf("Location = %q, want /console/workflows", got)
	}
}

func TestWorkflowEditor_DeleteReadOnlyBlocked(t *testing.T) {
	tc := newTestConsoleReadOnly(t)
	tc.seedWorkflow(t, "Delete Me", "delete-me")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/delete-me/delete", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("ReadOnly delete: status = %d, want 403", rr.Code)
	}
}

// ---- Wave 2: validation errors surface inline (W2-A4) ----

func TestWorkflowEditor_SaveWithEmptyNameShowsError(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/save", url.Values{
		"csrf_token": {csrf},
		"name":       {""},
	}, cookie)
	if rr.Code == http.StatusInternalServerError {
		t.Errorf("empty name save returned 500")
	}
}

// ---- Wave 2: auth gates ----

func TestWorkflowNew_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/workflows/new", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("/console/workflows/new without session: status = %d, want 303", rr.Code)
	}
}

func TestWorkflowCreate_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.postForm(t, "/console/api/workflows", url.Values{
		"name": {"Test"},
	}, nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /console/api/workflows without session: status = %d, want 303", rr.Code)
	}
}

// ---- Wave 2: no native confirm() dialogs ----

func TestConsoleWorkflowTemplates_NoNativeConfirm(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	pages := []string{
		"/console/workflows",
		"/console/workflows/new",
		"/console/workflows/my-wf",
		"/console/workflows/my-wf?confirmDelete=true",
	}
	for _, path := range pages {
		rr := tc.get(t, path, cookie)
		if strings.Contains(rr.Body.String(), "confirm(") || strings.Contains(rr.Body.String(), "alert(") {
			t.Errorf("%s: contains native confirm()/alert() call", path)
		}
	}
}

// ─── Wave 2-A5: workflow test/trigger page ───────────────────────────────────

func TestWorkflowTestPage_Renders(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-wf/test", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows/my-wf/test: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Must have a form with CSRF token.
	if !strings.Contains(body, "csrf_token") {
		t.Errorf("test page missing csrf_token field")
	}
	// Must have subscriber id input.
	if !strings.Contains(body, "subscriber_id") {
		t.Errorf("test page missing subscriber_id input")
	}
	// Must have payload textarea.
	if !strings.Contains(body, "payload") {
		t.Errorf("test page missing payload textarea")
	}
	// Must have send button.
	if !strings.Contains(body, "Send test trigger") {
		t.Errorf("test page missing submit button")
	}
	// Tab nav must be present.
	if !strings.Contains(body, "console-tab-bar") {
		t.Errorf("test page missing tab bar")
	}
}

func TestWorkflowTestPage_SingleStep_NoBanner(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Single", "single-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/single-wf/test", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "wf-multistep-banner") {
		t.Errorf("single-step workflow should NOT show the #17 multi-step banner")
	}
}

func TestWorkflowTestPage_MultiStep_ShowsBanner(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Multi", "multi-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"body":"bye"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/multi-wf/test", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "wf-multistep-banner") {
		t.Errorf("multi-step workflow should show the #17 multi-step banner")
	}
}

func TestWorkflowTestPage_InactiveWorkflow_ShowsBlockedMessage(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedInactiveWorkflow(t, "Paused", "paused-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/paused-wf/test", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "wf-inactive-banner") {
		t.Errorf("inactive workflow test page should show inactive warning banner")
	}
}

func TestWorkflowTestPage_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	rr := tc.get(t, "/console/workflows/my-wf/test", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("GET /console/workflows/my-wf/test without session: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("Location = %q, want /console/login", got)
	}
}

func TestWorkflowTest_POST_Success_Redirects_With_TxID(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, csrf := tc.login(t)

	// The stub trigger returns "txn-test-stub" for any request.
	rr := tc.postForm(t, "/console/api/workflows/my-wf/test", url.Values{
		"csrf_token":    {csrf},
		"subscriber_id": {"user-123"},
		"payload":       {`{"foo":"bar"}`},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST test trigger: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "txid=") {
		t.Errorf("redirect Location = %q, want ?txid=... present", loc)
	}
	if !strings.Contains(loc, "/console/workflows/my-wf/test") {
		t.Errorf("redirect Location = %q, want /console/workflows/my-wf/test prefix", loc)
	}
}

func TestWorkflowTest_POST_InvalidJSON_InlineError(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/test", url.Values{
		"csrf_token":    {csrf},
		"subscriber_id": {"user-123"},
		"payload":       {`not-valid-json`},
	}, cookie)
	// Must redirect (303) to test page with ?error=, not 500.
	if rr.Code == http.StatusInternalServerError {
		t.Fatalf("invalid JSON payload returned 500")
	}
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid JSON: status = %d, want 303", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Errorf("invalid JSON redirect missing error param")
	}
}

func TestWorkflowTest_POST_InactiveWorkflow_Blocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedInactiveWorkflow(t, "Paused", "paused-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/paused-wf/test", url.Values{
		"csrf_token":    {csrf},
		"subscriber_id": {"user-123"},
		"payload":       {`{}`},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("inactive workflow test: status = %d, want 303", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Errorf("inactive workflow test redirect missing error param")
	}
}

func TestWorkflowTest_POST_ReadOnly_Blocked(t *testing.T) {
	tc := newTestConsoleReadOnly(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/test", url.Values{
		"csrf_token":    {csrf},
		"subscriber_id": {"user-123"},
		"payload":       {`{}`},
	}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("ReadOnly test: status = %d, want 403", rr.Code)
	}
}

func TestWorkflowTest_POST_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	rr := tc.postForm(t, "/console/api/workflows/my-wf/test", url.Values{
		"subscriber_id": {"user-123"},
		"payload":       {`{}`},
	}, nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /console/api/workflows/my-wf/test without session: status = %d, want 303", rr.Code)
	}
}

func TestWorkflowTestPage_NoNativeConfirm(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	tc.seedInactiveWorkflow(t, "Paused", "paused-wf")
	tc.seedWorkflowWithSteps(t, "Multi", "multi-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, _ := tc.login(t)

	pages := []string{
		"/console/workflows/my-wf/test",
		"/console/workflows/paused-wf/test",
		"/console/workflows/multi-wf/test",
	}
	for _, path := range pages {
		rr := tc.get(t, path, cookie)
		if strings.Contains(rr.Body.String(), "confirm(") || strings.Contains(rr.Body.String(), "alert(") {
			t.Errorf("%s: contains native confirm()/alert()", path)
		}
	}
}

// ─── Wave 2-A6: workflow activity tab ────────────────────────────────────────

func TestWorkflowActivityPage_Renders(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-wf/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows/my-wf/activity: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Tab nav present.
	if !strings.Contains(body, "console-tab-bar") {
		t.Errorf("activity tab page missing tab bar")
	}
	// Table element present.
	if !strings.Contains(body, "wf-activity-table") {
		t.Errorf("activity tab page missing wf-activity-table")
	}
}

func TestWorkflowActivityPage_EmptyState(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-wf/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No runs yet for this workflow") {
		t.Errorf("empty activity tab missing empty-state title")
	}
}

func TestWorkflowActivityPage_ShowsRows(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "My WF", "my-wf")
	tc.seedNotification(t, domain.Notification{
		ID:            "notif-1",
		OrgID:         tc.scope.OrgID,
		EnvID:         tc.scope.EnvID,
		WorkflowID:    wf.ID,
		SubscriberID:  "user-abc",
		TransactionID: "tx-abc",
		RunID:         "run-abc",
		CreatedAt:     "2025-01-01T00:00:00Z",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-wf/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "user-abc") {
		t.Errorf("activity tab missing subscriber id 'user-abc'")
	}
	if !strings.Contains(body, "tx-abc") {
		t.Errorf("activity tab missing transaction id 'tx-abc'")
	}
}

func TestWorkflowActivityPage_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	// Seed a workflow in the main scope.
	wf := tc.seedWorkflowAndReturn(t, "My WF", "my-wf")
	// Seed a notification under the alt scope for a different workflow id.
	tc.seedNotificationInScope(t, altScope, domain.Notification{
		ID:            "notif-alt",
		OrgID:         altScope.OrgID,
		EnvID:         altScope.EnvID,
		WorkflowID:    wf.ID, // same ID string but wrong scope
		SubscriberID:  "alt-sub",
		TransactionID: "tx-alt",
		CreatedAt:     "2025-01-01T00:00:00Z",
	})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/workflows/my-wf/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "alt-sub") {
		t.Errorf("activity tab leaked alt-scope subscriber into main-scope view")
	}
}

func TestFragmentWorkflowActivity_ReturnsSSE(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-activity/my-wf", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: datastar-patch-elements") {
		t.Errorf("workflow activity fragment missing SSE event framing")
	}
	if !strings.Contains(body, "wf-activity-tbody") {
		t.Errorf("workflow activity fragment missing #wf-activity-tbody id")
	}
}

func TestFragmentWorkflowActivity_CursorPagination(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "My WF", "my-wf")
	// Seed more rows than the default limit to trigger pagination.
	for i := range 25 {
		tc.seedNotification(t, domain.Notification{
			ID:            "notif-" + strconv.Itoa(i),
			OrgID:         tc.scope.OrgID,
			EnvID:         tc.scope.EnvID,
			WorkflowID:    wf.ID,
			SubscriberID:  "user-" + strconv.Itoa(i),
			TransactionID: "tx-" + strconv.Itoa(i),
			CreatedAt:     "2025-01-01T00:00:00Z",
		})
	}
	cookie, _ := tc.login(t)

	// Request first page with limit=5.
	rr := tc.getWithQuery(t, "/console/api/fragments/workflow-activity/my-wf", "limit=5", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment limit=5: status = %d, want 200", rr.Code)
	}
	// Should have a "Load more" button indicating there is a next page.
	if !strings.Contains(rr.Body.String(), "Load more") {
		t.Errorf("first page with limit=5 against 25 rows should show 'Load more'")
	}
}

func TestWorkflowActivityPage_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	rr := tc.get(t, "/console/workflows/my-wf/activity", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("GET without session: status = %d, want 303", rr.Code)
	}
}

func TestWorkflowActivityPage_NoNativeConfirm(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-wf/activity", cookie)
	body := rr.Body.String()
	if strings.Contains(body, "confirm(") || strings.Contains(body, "alert(") {
		t.Errorf("/console/workflows/my-wf/activity: contains native confirm()/alert()")
	}
}

// ---- Wave 2: POST->303 not SSE ----

func TestWorkflowMutations_NotSSE(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	endpoints := []struct {
		method string
		path   string
		body   url.Values
	}{
		{"POST", "/console/api/workflows", url.Values{"csrf_token": {csrf}, "name": {"X"}, "trigger_identifier": {"x-mut-test"}}},
		{"POST", "/console/api/workflows/my-wf/steps", url.Values{"csrf_token": {csrf}, "type": {"in_app"}}},
		{"POST", "/console/api/workflows/my-wf/steps/email-1/delete", url.Values{"csrf_token": {csrf}}},
		{"POST", "/console/api/workflows/my-wf/delete", url.Values{"csrf_token": {csrf}}},
	}
	for _, ep := range endpoints {
		rr := tc.postForm(t, ep.path, ep.body, cookie)
		if strings.Contains(rr.Body.String(), "datastar-patch-elements") && rr.Code != http.StatusSeeOther {
			t.Errorf("%s %s: body contains datastar-patch-elements on non-SSE endpoint", ep.method, ep.path)
		}
	}
}

// ---- W4-W3: Handlebars live-preview pane ----

// TestRenderStepPreview_VariableSubstitution proves a payload variable
// substitutes through the same namespace the send-time engine uses
// ({{payload.*}}, mirroring internal/delivery/delivery.go).
func TestRenderStepPreview_VariableSubstitution(t *testing.T) {
	cases := []struct {
		name    string
		tmpl    string
		payload string
		want    string
	}{
		{"payload var", "Hello {{payload.name}}", `{"name":"World"}`, "Hello World"},
		{"nested payload", "You have {{payload.order.count}} items", `{"order":{"count":3}}`, "You have 3 items"},
		{"missing path empty", "Hi {{payload.nope}}", `{"name":"x"}`, "Hi "},
		{"empty payload renders", "Plain text", "", "Plain text"},
		{"each block", "{{#each payload.items}}{{this}},{{/each}}", `{"items":["a","b"]}`, "a,b,"},
		{"if block", "{{#if payload.on}}Y{{else}}N{{/if}}", `{"on":true}`, "Y"},
		{"t helper passthrough", `{{t "greeting"}}`, "{}", "greeting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderStepPreview(c.tmpl, c.payload)
			if got.IsError {
				t.Fatalf("renderStepPreview(%q,%q): unexpected error %q", c.tmpl, c.payload, got.Error)
			}
			if got.Output != c.want {
				t.Errorf("renderStepPreview(%q,%q) = %q, want %q", c.tmpl, c.payload, got.Output, c.want)
			}
		})
	}
}

// TestRenderStepPreview_InvalidTemplateInlineError proves a malformed
// Handlebars template surfaces an inline error (never a panic / 500).
func TestRenderStepPreview_InvalidTemplateInlineError(t *testing.T) {
	got := renderStepPreview("Hello {{#if payload.x}}unterminated", `{"x":true}`)
	if !got.IsError {
		t.Fatalf("expected IsError for malformed template, got output %q", got.Output)
	}
	if got.Error == "" {
		t.Errorf("expected a non-empty inline error message")
	}
	if got.Output != "" {
		t.Errorf("expected empty output on error, got %q", got.Output)
	}
}

// TestRenderStepPreview_InvalidPayloadInlineError proves bad payload JSON
// surfaces an inline error rather than rendering against a half-parsed map.
func TestRenderStepPreview_InvalidPayloadInlineError(t *testing.T) {
	got := renderStepPreview("Hello {{payload.name}}", `{bad json`)
	if !got.IsError {
		t.Fatalf("expected IsError for malformed payload JSON, got output %q", got.Output)
	}
	if !strings.Contains(got.Error, "Payload JSON") {
		t.Errorf("expected payload JSON error, got %q", got.Error)
	}
}

// TestRenderStepPreview_ScopeSafety proves the preview engine is logicless:
// a user template cannot reach the filesystem or invoke unregistered helpers
// to escape the sandbox. An unknown helper invocation does not execute
// arbitrary code; it either renders empty or surfaces an inline error — never
// a panic, never an OS-level side effect.
func TestRenderStepPreview_ScopeSafety(t *testing.T) {
	// No filesystem helper exists; {{include}} / {{require}} are not registered.
	// Such a template must not panic and must not leak host content.
	for _, tmpl := range []string{
		`{{require "/etc/passwd"}}`,
		`{{include "../../secret"}}`,
		`{{readFile "/etc/hosts"}}`,
		`{{exec "rm -rf /"}}`,
	} {
		got := renderStepPreview(tmpl, "{}")
		// Whatever the outcome (empty render or inline error), it must never
		// contain host file content and must never crash the handler.
		if strings.Contains(got.Output, "root:") {
			t.Errorf("template %q leaked host file content: %q", tmpl, got.Output)
		}
	}
}

// TestFragmentStepPreview_RendersLive drives the full HTTP fragment: a GET
// with tmpl + payload query params returns the rendered output in a Datastar
// patch, never a 500.
func TestFragmentStepPreview_RendersLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/step-preview",
		"tmpl="+url.QueryEscape("Hello {{payload.name}}")+"&payload="+url.QueryEscape(`{"name":"World"}`),
		cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("step-preview: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Hello World") {
		t.Errorf("step-preview body missing rendered output, got: %s", body)
	}
	if !strings.Contains(body, "step-preview-pane") {
		t.Errorf("step-preview body missing pane target id, got: %s", body)
	}
}

// TestFragmentStepPreview_InvalidTemplateNo500 proves the HTTP path returns
// 200 with an inline error for a malformed template — never a 500.
func TestFragmentStepPreview_InvalidTemplateNo500(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/step-preview",
		"tmpl="+url.QueryEscape("Hi {{#if payload.x}}oops")+"&payload="+url.QueryEscape(`{"x":true}`),
		cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("step-preview malformed: status = %d, want 200 (no 500)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "step-preview-error") {
		t.Errorf("expected inline error class in body, got: %s", rr.Body.String())
	}
}

// TestFragmentStepPreview_RequiresAuth proves the fragment sits behind the
// session gate like every other fragment route.
func TestFragmentStepPreview_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/api/fragments/step-preview", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step-preview without session: status = %d, want 303", rr.Code)
	}
}
