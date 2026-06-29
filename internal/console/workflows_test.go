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
	"github.com/danmestas/notify/internal/delivery/filter"
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

// ---- regression: Bug 1 — search input must use colon-form data-on:input ----

// TestWorkflowsList_SearchInputUsesColonEventSyntax guards against the
// hyphen-form regression (data-on-input vs data-on:input).  The Datastar build
// used by this project only processes the colon form; the hyphen form silently
// does nothing, making the search box dead.
func TestWorkflowsList_SearchInputUsesColonEventSyntax(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The search input must carry the colon form so Datastar picks it up.
	if !strings.Contains(body, `data-on:input__debounce.300ms`) {
		t.Errorf("search input missing data-on:input__debounce.300ms (colon form); Datastar will not fire the handler")
	}
	// The broken hyphen form must not appear anywhere on the page.
	if strings.Contains(body, `data-on-input`) {
		t.Errorf("page contains data-on-input (hyphen form); this handler will never fire in the current Datastar build")
	}
}

// ---- regression: Bug 2 — duplicate POST success must not redirect to ?error ----

// ---- regression: Bug 3 — step-editor wiring on workflow_detail.html ----

// TestWorkflowDetail_StepEditorWiringPresent proves the detail page wires each
// step item to @get the step-editor fragment and provides a #workflow-step-editor
// target container for Datastar PatchElements to land in. Regression for Run-2
// finding: the served page had zero references to 'workflow-step-editor'.
func TestWorkflowDetail_StepEditorWiringPresent(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Hello"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-wf", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows/my-wf: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Each step item must embed an @get URL pointing at the step-editor fragment.
	if !strings.Contains(body, "workflow-step-editor/my-wf/email-1") {
		t.Errorf("detail page step item missing @get URL for step-editor fragment (my-wf/email-1)")
	}
	// A target container must be present for Datastar PatchElements to replace.
	if !strings.Contains(body, `id="workflow-step-editor"`) {
		t.Errorf("detail page missing #workflow-step-editor target container for PatchElements")
	}
}

// TestFragmentWorkflowStepEditor_RendersEmailFields proves the step-editor
// fragment endpoint returns Datastar SSE with per-type fields for an email step.
func TestFragmentWorkflowStepEditor_RendersEmailFields(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Hello World","body":"Hi there"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/email-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET step-editor fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Must be Datastar SSE.
	if !strings.Contains(body, "event: datastar-patch-elements") {
		t.Errorf("step-editor fragment missing SSE event framing")
	}
	// Must carry the target element id.
	if !strings.Contains(body, "workflow-step-editor") {
		t.Errorf("step-editor fragment missing #workflow-step-editor id in response")
	}
	// Must render email-specific subject field.
	if !strings.Contains(body, `name="subject"`) {
		t.Errorf("email step-editor fragment missing subject field")
	}
	// Pre-filled value from controlValues must appear.
	if !strings.Contains(body, "Hello World") {
		t.Errorf("email step-editor fragment missing pre-filled subject value")
	}
}

// TestFragmentWorkflowStepEditor_RequiresAuth proves the fragment endpoint sits
// behind the session gate.
func TestFragmentWorkflowStepEditor_RequiresAuth(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/some-step", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step-editor fragment without session: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("Location = %q, want /console/login", got)
	}
}

// TestWorkflowDuplicate_SuccessNeverRedirectsToError is the regression for the
// bug where authoring.Duplicate returned (Workflow{}, err) after a successful
// Create+Publish sequence, causing handleWorkflowDuplicate to redirect to
// ?error=Duplicate+failed even though the workflow was persisted.  The handler
// must always redirect to the new editor on a clean unique-identifier POST.
func TestWorkflowDuplicate_SuccessNeverRedirectsToError(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Source WF", "source-wf")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/source-wf/duplicate", url.Values{
		"csrf_token":         {csrf},
		"name":               {"Copy of Source WF"},
		"trigger_identifier": {"source-wf-copy-unique"},
	}, cookie)

	// Must be a success redirect, never an error redirect.
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("duplicate POST: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if strings.Contains(loc, "error=") {
		t.Errorf("duplicate success redirected to an error URL: %q", loc)
	}
	if loc != "/console/workflows/source-wf-copy-unique" {
		t.Errorf("Location = %q, want /console/workflows/source-wf-copy-unique", loc)
	}
}

// ─── regression: Bug 1 — search bind must use the VALUE form ─────────────────

// TestWorkflowsList_SearchBindUsesValueForm guards the attribute-suffix
// regression: data-bind-wfFilter loses its capital F in the browser
// (HTML attribute names are case-folded to lowercase), so Datastar sees
// the signal "wffilter" instead of "wfFilter" and the typed value never
// reaches the server. The VALUE form data-bind="wfFilter" preserves the
// original case because HTML preserves attribute values.
func TestWorkflowsList_SearchBindUsesValueForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Value form must be present — Datastar reads signal name from the attribute
	// VALUE, which the browser does NOT case-fold.
	if !strings.Contains(body, `data-bind="wfFilter"`) {
		t.Errorf("search input missing data-bind value form; Datastar will see the wrong signal name")
	}
	// Attribute-suffix form must NOT appear — the browser lowercases it to
	// data-bind-wffilter, producing signal "wffilter" ≠ "wfFilter".
	if strings.Contains(body, `data-bind-wfFilter`) || strings.Contains(body, `data-bind-wffilter`) {
		t.Errorf("search input must not use attribute-suffix data-bind-wfFilter form (case-lossy)")
	}
}

// TestFragmentWorkflowsList_SearchFilterEndToEnd proves that after the
// correct data-bind="wfFilter" fix, the wfFilter signal (mixed-case key)
// actually reaches the server and filters the list.  This duplicates
// TestFragmentWorkflowsList_SearchFilter at the wire level but makes the
// causal chain explicit.
func TestFragmentWorkflowsList_SearchFilterEndToEnd(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Welcome", "welcome-email")
	tc.seedWorkflow(t, "Digest", "daily-digest")
	cookie, _ := tc.login(t)

	// Signal sent with the correct mixed-case key as Datastar emits it.
	v := url.Values{}
	v.Set("datastar", `{"wfFilter":"welcome","wfStatus":"","wfConfirmDelete":""}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/workflows-list", v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Welcome") {
		t.Errorf("filter end-to-end: fragment missing Welcome row")
	}
	if strings.Contains(body, "Digest") {
		t.Errorf("filter end-to-end: fragment should not contain Digest row (filtered out)")
	}
}

// ─── regression: Bug 2 — delete kebab must use data-on:click, not bare href ──

// TestWorkflowsList_DeleteKebabUsesDatastarClick proves the Delete item in
// the row kebab menu is wired as a Datastar in-place update (data-on:click)
// rather than a bare anchor that navigates to the SSE fragment endpoint.
// A bare href causes full-page navigation to the SSE stream, dumping raw
// "event: datastar-patch-elements" text in the browser.
func TestWorkflowsList_DeleteKebabUsesDatastarClick(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The delete action must use Datastar's event handler.
	if !strings.Contains(body, `data-on:click`) {
		t.Errorf("delete kebab missing data-on:click — must use Datastar in-place update")
	}
	// Must NOT be a bare href pointing at the SSE fragment endpoint.
	if strings.Contains(body, `href="/console/api/fragments/workflows-list?confirmDelete=`) {
		t.Errorf("delete kebab must not be a bare anchor navigating to the fragment SSE endpoint")
	}
}

// TestFragmentWorkflowsList_ConfirmDeleteViaSignal proves the confirmDelete
// state is correctly rendered when delivered through the wfConfirmDelete
// signal (the path taken by data-on:click rather than a query-param href).
func TestFragmentWorkflowsList_ConfirmDeleteViaSignal(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "My WF", "my-wf")
	cookie, _ := tc.login(t)

	v := url.Values{}
	v.Set("datastar", `{"wfFilter":"","wfStatus":"","wfConfirmDelete":"my-wf"}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/workflows-list", v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm-via-signal: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// The confirm row UI must appear inside the fragment.
	if !strings.Contains(body, "console-confirm-label") {
		t.Errorf("confirm-via-signal: fragment missing console-confirm-label confirm row")
	}
	if !strings.Contains(body, "Delete?") {
		t.Errorf("confirm-via-signal: fragment missing Delete? label")
	}
}

// ─── regression: Bug 3 — footer and Reset control ────────────────────────────

// TestWorkflowsList_FooterHasIdAndShowsMatchedOfTotal proves the footer
// carries a stable id (so Datastar PatchElements can update it in place)
// and shows both the filtered row count and the total count.
func TestWorkflowsList_FooterHasIdAndShowsMatchedOfTotal(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Alpha", "alpha")
	tc.seedWorkflow(t, "Beta", "beta")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// Footer must carry a stable id for Datastar in-place patching.
	if !strings.Contains(body, `id="wf-footer-note"`) {
		t.Errorf("footer missing id=\"wf-footer-note\" — Datastar cannot patch it without a stable id")
	}
	// Unfiltered: 2 of 2.
	if !strings.Contains(body, "2 of 2") {
		t.Errorf("footer does not show matched-of-total (expected '2 of 2')")
	}
	// The old always-same-number form (len of len) must not appear.
	if strings.Contains(body, "2 of 2 of") {
		t.Errorf("footer shows triple-redundant count")
	}
}

// TestFragmentWorkflowsList_FooterReflectsFilteredCount proves that when a
// filter is active, the fragment patches the footer with the correct
// matched-vs-total ratio.
func TestFragmentWorkflowsList_FooterReflectsFilteredCount(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "Alpha", "alpha")
	tc.seedWorkflow(t, "Beta", "beta")
	cookie, _ := tc.login(t)

	v := url.Values{}
	v.Set("datastar", `{"wfFilter":"alpha","wfStatus":"","wfConfirmDelete":""}`)
	rr := tc.getWithQuery(t, "/console/api/fragments/workflows-list", v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment filter: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// 1 matched, 2 total.
	if !strings.Contains(body, "1 of 2") {
		t.Errorf("fragment footer does not show '1 of 2' for filtered result (matched of total)")
	}
	// Footer element must be in the fragment so Datastar patches it.
	if !strings.Contains(body, `id="wf-footer-note"`) {
		t.Errorf("fragment missing id=\"wf-footer-note\" — footer will not update on filter")
	}
}

// TestWorkflowsList_ResetControlPresent proves a Reset control with a
// data-show attribute is rendered in the filter bar so it is visible
// exactly when filter signals are non-empty.
func TestWorkflowsList_ResetControlPresent(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// A Reset control must be present in the rendered HTML (visibility is
	// controlled client-side via data-show on the filter signals).
	if !strings.Contains(body, "Reset") {
		t.Errorf("filter bar missing Reset control")
	}
	// It must be hidden/shown reactively via data-show (not a server-side {{if}}).
	if !strings.Contains(body, `data-show=`) {
		t.Errorf("Reset control missing data-show attribute for reactive visibility")
	}
}

// ─── regression: Bug 4 — chat step editor ────────────────────────────────────

// TestFragmentWorkflowStepEditor_RendersChat proves a chat step renders a
// body/message textarea (mirroring sms) rather than falling through to the
// generic JSON control-values editor.
func TestFragmentWorkflowStepEditor_RendersChat(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"Hello chat"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/chat-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET step-editor chat fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	if !strings.Contains(body, "event: datastar-patch-elements") {
		t.Errorf("chat step-editor fragment missing SSE event framing")
	}
	// Must render a body/message textarea (not the generic JSON fallback).
	if !strings.Contains(body, `name="body"`) {
		t.Errorf("chat step-editor fragment missing body textarea (fell through to generic editor)")
	}
	// Generic JSON fallback must NOT appear for chat.
	if strings.Contains(body, `name="control_values_json"`) {
		t.Errorf("chat step-editor fragment fell through to generic JSON editor instead of typed form")
	}
	// Pre-filled controlValues body must appear.
	if !strings.Contains(body, "Hello chat") {
		t.Errorf("chat step-editor fragment missing pre-filled body value from controlValues")
	}
}

// TestWorkflowStepSave_ChatSavesBody proves the chat step-save handler
// extracts body from the form (not control_values_json) and persists it.
func TestWorkflowStepSave_ChatSavesBody(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"old"}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/chat-1/save", url.Values{
		"csrf_token": {csrf},
		"body":       {"new chat body"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("chat step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	// Re-fetch the step editor to confirm the value was persisted.
	rr2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/chat-1", cookie)
	if !strings.Contains(rr2.Body.String(), "new chat body") {
		t.Errorf("after chat step save, step editor missing updated body value")
	}
}

// ─── regression: Bug 5 — email/in_app/push content must round-trip ───────────

// TestWorkflowStepSave_EmailSubjectAndBodyRoundTrip proves the email step-save
// handler persists BOTH the subject and the body posted by the per-type form,
// and that the step editor re-renders them on reload. Regression for the
// content-bearing step types (email/in_app/push) losing their controlValues.
func TestWorkflowStepSave_EmailSubjectAndBodyRoundTrip(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/email-1/save", url.Values{
		"csrf_token": {csrf},
		"subject":    {"Welcome aboard"},
		"body":       {"Hello {{payload.name}}"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("email step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	// Reload the step editor; both fields must repopulate from the stored
	// controlValues (save -> reload -> repopulate).
	rr2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/email-1", cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, `value="Welcome aboard"`) {
		t.Errorf("after email save, subject field did not repopulate; want value=\"Welcome aboard\"")
	}
	if !strings.Contains(body, "Hello ") || !strings.Contains(body, "payload.name") {
		t.Errorf("after email save, body textarea did not repopulate with the saved body")
	}
}

// TestWorkflowStepSave_PushTitleRoundTrip proves the push step-save handler
// persists the title (and body) and the editor repopulates them on reload.
func TestWorkflowStepSave_PushTitleRoundTrip(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "push-1", Type: "push", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/push-1/save", url.Values{
		"csrf_token": {csrf},
		"title":      {"Ping title"},
		"body":       {"Ping body"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("push step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/push-1", cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, `value="Ping title"`) {
		t.Errorf("after push save, title field did not repopulate; want value=\"Ping title\"")
	}
	if !strings.Contains(body, "Ping body") {
		t.Errorf("after push save, body textarea did not repopulate with the saved body")
	}
}

// TestFragmentWorkflowStepEditor_SeedsPreviewSignalWithBody proves the
// step-editor fragment seeds the wfPreviewTmpl signal with the step's saved
// body. The body textarea carries data-bind="wfPreviewTmpl"; if that signal is
// pre-declared empty (wfPreviewTmpl:”), Datastar's two-way binding overwrites
// the server-rendered textarea content with the empty signal on load — wiping
// the saved body and persisting empty on the next save. Seeding the signal with
// the body keeps the textarea populated.
func TestFragmentWorkflowStepEditor_SeedsPreviewSignalWithBody(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Hi","body":"BODYSEED"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/email-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET step-editor fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The empty-seed form must NOT be present — it is what wipes the body.
	if strings.Contains(body, "wfPreviewTmpl:''") {
		t.Errorf("step-editor fragment pre-declares wfPreviewTmpl empty; data-bind will wipe the saved body")
	}
	// The signal must be seeded with the step's saved body.
	if !strings.Contains(body, "wfPreviewTmpl:&#39;BODYSEED&#39;") {
		t.Errorf("step-editor fragment did not seed wfPreviewTmpl with the saved body (want wfPreviewTmpl:'BODYSEED')")
	}
}

// ─── regression: Bug 6 — Activate control must be a real form submit ─────────

// TestWorkflowEditor_ActivateButtonIsRealSubmit proves the editor's Status
// section renders the Activate control as a genuine submit button inside a
// <form> that POSTs active=1 to the activate endpoint — so clicking it actually
// fires the activation (rather than a stray button outside the form, a
// non-submit, or a non-reactive Datastar attribute).
func TestWorkflowEditor_ActivateButtonIsRealSubmit(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedInactiveWorkflow(t, "Toggle WF", "toggle-wf")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/toggle-wf", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET inactive detail: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Isolate the activate <form> so the assertions are scoped to it.
	const action = `action="/console/api/workflows/toggle-wf/activate"`
	start := strings.Index(body, action)
	if start == -1 {
		t.Fatalf("inactive detail missing activate form action %q", action)
	}
	formEnd := strings.Index(body[start:], "</form>")
	if formEnd == -1 {
		t.Fatalf("activate form not closed after action")
	}
	form := body[start : start+formEnd]

	// The activate value must be 1 (activate, not deactivate).
	if !strings.Contains(form, `name="active" value="1"`) {
		t.Errorf("activate form does not POST active=1; got form: %q", form)
	}
	// The Activate control must be a real submit button inside this form.
	if !strings.Contains(form, `type="submit"`) {
		t.Errorf("activate form has no submit button; clicking Activate will not fire")
	}
	if !strings.Contains(form, "Activate") {
		t.Errorf("activate form missing the Activate label")
	}
}

// ─── per-step CONDITIONS authoring (step.Filters) ────────────────────────────

// TestWorkflowStepEditor_RendersConditionsSection proves the step editor
// renders a "When should this step run?" conditions fieldset with the
// combinator select + at least one condition row (On / Field / Operator /
// Value), using design-system controls.
func TestWorkflowStepEditor_RendersConditionsSection(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET step-editor fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	if !strings.Contains(body, "When should this step run?") {
		t.Errorf("step editor missing the conditions fieldset legend")
	}
	// Combinator select posts cond_combinator.
	if !strings.Contains(body, `name="cond_combinator"`) {
		t.Errorf("step editor missing combinator select (name=cond_combinator)")
	}
	// At least the first condition row's four fields must be present.
	for _, name := range []string{"cond_on_0", "cond_field_0", "cond_operator_0", "cond_value_0"} {
		if !strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("step editor missing condition field name=%q", name)
		}
	}
}

// TestWorkflowStepSave_ConditionPersistsFilters proves that posting a single
// condition row persists the correct []filter.Group JSON into step.Filters.
func TestWorkflowStepSave_ConditionPersistsFilters(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"subscriber"},
		"cond_field_0":    {"data.plan"},
		"cond_operator_0": {"EQUAL"},
		"cond_value_0":    {"premium"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step save with condition: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	// Read the persisted step.Filters back out of the store and assert it
	// matches the shared filter contract exactly.
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		t.Fatalf("decodeSteps: %v", err)
	}
	var saved domain.Step
	for _, s := range steps {
		if s.StepID == "in_app-1" {
			saved = s
		}
	}
	if len(saved.Filters) == 0 {
		t.Fatalf("step.Filters empty after saving a condition")
	}
	var groups []filter.Group
	if err := json.Unmarshal(saved.Filters, &groups); err != nil {
		t.Fatalf("step.Filters is not []filter.Group: %v (raw: %s)", err, saved.Filters)
	}
	want := []filter.Group{{
		Combinator: "AND",
		Conditions: []filter.Condition{{
			On:       "subscriber",
			Field:    "data.plan",
			Operator: filter.OpEqual,
			Value:    "premium",
		}},
	}}
	if len(groups) != 1 || len(groups[0].Conditions) != 1 {
		t.Fatalf("step.Filters shape = %+v, want one group with one condition", groups)
	}
	gotC := groups[0].Conditions[0]
	wantC := want[0].Conditions[0]
	if groups[0].Combinator != want[0].Combinator {
		t.Errorf("combinator = %q, want %q", groups[0].Combinator, want[0].Combinator)
	}
	if gotC.On != wantC.On || gotC.Field != wantC.Field || gotC.Operator != wantC.Operator {
		t.Errorf("condition on/field/operator = %q/%q/%q, want %q/%q/%q",
			gotC.On, gotC.Field, gotC.Operator, wantC.On, wantC.Field, wantC.Operator)
	}
	if v, ok := gotC.Value.(string); !ok || v != "premium" {
		t.Errorf("condition value = %#v, want string \"premium\"", gotC.Value)
	}

	// Literal-key wire-format assertion: the JSON must carry the exact contract keys.
	raw := string(saved.Filters)
	for _, key := range []string{`"combinator":"AND"`, `"on":"subscriber"`, `"field":"data.plan"`, `"operator":"EQUAL"`, `"value":"premium"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("step.Filters JSON missing literal key %s; raw = %s", key, raw)
		}
	}
}

// TestWorkflowStepSave_ConditionRoundTrips proves saving a condition then
// reloading the step editor repopulates the condition row fields.
func TestWorkflowStepSave_ConditionRoundTrips(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"OR"},
		"cond_on_0":       {"payload"},
		"cond_field_0":    {"data.plan"},
		"cond_operator_0": {"EQUAL"},
		"cond_value_0":    {"premium"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie)
	body := rr2.Body.String()
	// Field + value prefilled into the text inputs.
	if !strings.Contains(body, `value="data.plan"`) {
		t.Errorf("after save, condition Field did not repopulate (want value=\"data.plan\"); body: %s", body)
	}
	if !strings.Contains(body, `value="premium"`) {
		t.Errorf("after save, condition Value did not repopulate (want value=\"premium\")")
	}
	// The combinator + on + operator selects seed their value signals to the
	// saved choices (uiSelectForm declares initialValue in data-signals).
	if !strings.Contains(body, "OR") {
		t.Errorf("after save, combinator did not repopulate to OR")
	}
	if !strings.Contains(body, "payload") {
		t.Errorf("after save, condition On did not repopulate to payload")
	}
}

// TestWorkflowStepSave_EmptyConditionsLeavesFiltersAbsent proves that saving
// with no condition rows filled in leaves step.Filters empty/absent, so the
// step is not gated.
func TestWorkflowStepSave_EmptyConditionsLeavesFiltersAbsent(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"subscriber"},
		"cond_field_0":    {""},
		"cond_operator_0": {"EQUAL"},
		"cond_value_0":    {""},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		t.Fatalf("decodeSteps: %v", err)
	}
	for _, s := range steps {
		if s.StepID == "in_app-1" && len(s.Filters) != 0 {
			t.Errorf("step.Filters = %s, want empty/absent when no conditions entered", s.Filters)
		}
	}
}

// savedStepFilters posts a single condition row and reads the resulting
// step.Filters []filter.Group back out of the store.
func savedStepFilters(t *testing.T, on, field, operator, value string) []filter.Group {
	t.Helper()
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {on},
		"cond_field_0":    {field},
		"cond_operator_0": {operator},
		"cond_value_0":    {value},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		t.Fatalf("decodeSteps: %v", err)
	}
	var saved domain.Step
	for _, s := range steps {
		if s.StepID == "in_app-1" {
			saved = s
		}
	}
	if len(saved.Filters) == 0 {
		t.Fatalf("step.Filters empty after saving a condition")
	}
	var groups []filter.Group
	if err := json.Unmarshal(saved.Filters, &groups); err != nil {
		t.Fatalf("step.Filters is not []filter.Group: %v (raw: %s)", err, saved.Filters)
	}
	return groups
}

// TestWorkflowStepSave_InValueParsedAsCommaList proves operator=IN with a
// comma-separated Value persists a []any list of trimmed string elements.
func TestWorkflowStepSave_InValueParsedAsCommaList(t *testing.T) {
	groups := savedStepFilters(t, "payload", "data.plan", "IN", "a, b ,c")
	if len(groups) != 1 || len(groups[0].Conditions) != 1 {
		t.Fatalf("step.Filters shape = %+v, want one group with one condition", groups)
	}
	got, ok := groups[0].Conditions[0].Value.([]any)
	if !ok {
		t.Fatalf("IN value = %#v, want []any", groups[0].Conditions[0].Value)
	}
	want := []any{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("IN value = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IN value[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestWorkflowStepSave_BetweenValueParsedAsNumberRange proves operator=BETWEEN
// with "1,10" persists a 2-element numeric []any the evaluator's between helper
// accepts.
func TestWorkflowStepSave_BetweenValueParsedAsNumberRange(t *testing.T) {
	groups := savedStepFilters(t, "payload", "data.count", "BETWEEN", "1,10")
	got, ok := groups[0].Conditions[0].Value.([]any)
	if !ok {
		t.Fatalf("BETWEEN value = %#v, want []any", groups[0].Conditions[0].Value)
	}
	want := []any{float64(1), float64(10)}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("BETWEEN value = %#v, want %#v", got, want)
	}

	// The produced shape must actually evaluate: a field of 5 is in [1,10].
	cond := groups[0].Conditions[0]
	cond.On = "payload"
	pass, err := filter.Match([]filter.Group{{Combinator: "AND", Conditions: []filter.Condition{cond}}},
		filter.EvalContext{Sources: map[string]map[string]any{"payload": {"data": map[string]any{"count": 5}}}})
	if err != nil {
		t.Fatalf("Match error: %v", err)
	}
	if !pass {
		t.Errorf("BETWEEN [1,10] did not match field value 5; produced shape not evaluator-compatible")
	}
}

// TestWorkflowStepSave_ListValueRoundTrips proves an IN list and a BETWEEN range
// render back into the single Value text input as comma-joined text.
func TestWorkflowStepSave_ListValueRoundTrips(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
		{StepID: "in_app-2", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	// IN list round-trip.
	if rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"payload"},
		"cond_field_0":    {"data.plan"},
		"cond_operator_0": {"IN"},
		"cond_value_0":    {"a,b,c"},
	}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("IN save: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie).Body.String()
	if !strings.Contains(body, `value="a,b,c"`) {
		t.Errorf("IN list did not round-trip to comma form value=\"a,b,c\"; body: %s", body)
	}

	// BETWEEN range round-trip.
	if rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-2/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"payload"},
		"cond_field_0":    {"data.count"},
		"cond_operator_0": {"BETWEEN"},
		"cond_value_0":    {"1,10"},
	}, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("BETWEEN save: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-2", cookie).Body.String()
	if !strings.Contains(body2, `value="1,10"`) {
		t.Errorf("BETWEEN range did not round-trip to \"1,10\"; body: %s", body2)
	}
}

// TestWorkflowStepSave_FiveConditionsRoundTrip proves the conditions UI is no
// longer capped at 3 rows: saving five condition rows persists a Group with
// five conditions, and reloading the editor renders all five pre-filled rows
// with the visible-row count signal seeded to 5.
func TestWorkflowStepSave_FiveConditionsRoundTrip(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	form := url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
	}
	fields := []string{"data.a", "data.b", "data.c", "data.d", "data.e"}
	for i, f := range fields {
		s := strconv.Itoa(i)
		form.Set("cond_on_"+s, "payload")
		form.Set("cond_field_"+s, f)
		form.Set("cond_operator_"+s, "EQUAL")
		form.Set("cond_value_"+s, "v"+s)
	}

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save five conditions: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		t.Fatalf("decodeSteps: %v", err)
	}
	var saved domain.Step
	for _, s := range steps {
		if s.StepID == "in_app-1" {
			saved = s
		}
	}
	var groups []filter.Group
	if err := json.Unmarshal(saved.Filters, &groups); err != nil {
		t.Fatalf("step.Filters is not []filter.Group: %v (raw: %s)", err, saved.Filters)
	}
	if len(groups) != 1 || len(groups[0].Conditions) != 5 {
		t.Fatalf("want one group with 5 conditions, got %+v", groups)
	}

	// Reload the editor: all five rows pre-filled.
	body := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie).Body.String()
	for _, f := range fields {
		if !strings.Contains(body, `value="`+f+`"`) {
			t.Errorf("reload did not pre-fill condition Field %q; body: %s", f, body)
		}
	}
	// Visible-row count signal seeded to 5.
	if !strings.Contains(body, "condRowCount") {
		t.Errorf("editor missing condRowCount signal")
	}
	if !strings.Contains(body, "condRowCount:5") && !strings.Contains(body, "condRowCount: 5") {
		t.Errorf("condRowCount not seeded to 5; body: %s", body)
	}
}

// TestWorkflowStepEditor_AddConditionControlPresent proves the editor exposes
// an "Add condition" affordance and the visible-row count signal, seeded to 1
// when a single condition was saved.
func TestWorkflowStepEditor_AddConditionControlPresent(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"payload"},
		"cond_field_0":    {"data.plan"},
		"cond_operator_0": {"EQUAL"},
		"cond_value_0":    {"premium"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	body := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie).Body.String()
	if !strings.Contains(body, "Add condition") {
		t.Errorf("editor missing the Add condition control; body: %s", body)
	}
	if !strings.Contains(body, "condRowCount") {
		t.Errorf("editor missing condRowCount signal")
	}
	if !strings.Contains(body, "condRowCount:1") && !strings.Contains(body, "condRowCount: 1") {
		t.Errorf("condRowCount not seeded to 1; body: %s", body)
	}
}

// TestStepConditionRow_ValueBindsSignal proves every per-row condition input is
// bound to a per-row signal (condOn{i}, condField{i}, condOperator{i},
// condValue{i}) so the remove handler's positional shift can move all four
// values up a slot. The Value input historically had no signal; the shift
// requires one. The submit name="cond_value_{i}" must remain so the server-side
// parse keeps reading the current value.
func TestStepConditionRow_ValueBindsSignal(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, _ := tc.login(t)

	body := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie).Body.String()

	// Value input keeps its submit name AND now binds a per-row signal.
	if !strings.Contains(body, `name="cond_value_0"`) {
		t.Errorf("Value input lost its submit name=cond_value_0; body: %s", body)
	}
	if !strings.Contains(body, `data-bind="condValue0"`) {
		t.Errorf("Value input must bind per-row signal data-bind=\"condValue0\"; body: %s", body)
	}
	// The condValue0 signal must be seeded (declared) so the shift can read it.
	if !strings.Contains(body, "condValue0:") {
		t.Errorf("condValue0 signal not seeded; body: %s", body)
	}
}

// TestStepConditionRemove_UsesPositionalShift proves the per-row remove (×)
// button performs a dense positional shift via Datastar signal assignments
// rather than the old blank-in-place behavior. Removing row 0 must copy row 1's
// four signals into row 0 (condField0 = condField1, etc.) and decrement the
// visible-row count. Without the shift a removed middle row leaves a visible
// hole and the wrong trailing slot collapses.
func TestStepConditionRemove_UsesPositionalShift(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, _ := tc.login(t)

	body := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie).Body.String()

	// Row 0's remove handler shifts row 1 up into row 0 for all four signals.
	for _, frag := range []string{
		"$condField0 = $condField1",
		"$condOn0 = $condOn1",
		"$condOperator0 = $condOperator1",
		"$condValue0 = $condValue1",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("remove handler missing shift assignment %q; body: %s", frag, body)
		}
	}
	// And it still decrements the visible-row count. The ">" is HTML-escaped to
	// "&gt;" in the attribute (the browser decodes it back); assert the rendered
	// form.
	if !strings.Contains(body, "condRowCount = ($condRowCount &gt; 1 ? $condRowCount - 1 : 1)") {
		t.Errorf("remove handler must still decrement condRowCount; body: %s", body)
	}
	// The old blank-in-place-only handler ($condFieldN = '' as the whole action)
	// must be gone; the shift replaces it.
	if strings.Contains(body, `data-on:click="$condField0 = ''; $condRowCount`) {
		t.Errorf("remove handler still uses the old blank-in-place form; body: %s", body)
	}
}

// TestWorkflowStepSave_RemoveMiddleRowDenseOrderPersists proves the round-trip
// contract the client-side shift produces: when the operator removes a MIDDLE
// row, the browser shifts later rows up into the vacated slot and clears the
// trailing slot, then submits a dense 1..N block. The server must persist those
// conditions in order with the removed one gone. This simulates the post-shift
// form state for rows [a,b,c] after removing the middle (b): [a,c,blank].
func TestWorkflowStepSave_RemoveMiddleRowDenseOrderPersists(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	// Post-shift state after removing middle row b from [a,b,c]: row0=a, row1=c,
	// row2 cleared. This is exactly what the dense positional shift submits.
	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"payload"},
		"cond_field_0":    {"data.a"},
		"cond_operator_0": {"EQUAL"},
		"cond_value_0":    {"av"},
		"cond_on_1":       {"payload"},
		"cond_field_1":    {"data.c"}, // c shifted up into the vacated middle slot
		"cond_operator_1": {"NOT_EQUAL"},
		"cond_value_1":    {"cv"},
		"cond_on_2":       {"payload"}, // trailing slot cleared by the shift
		"cond_field_2":    {""},
		"cond_operator_2": {"EQUAL"},
		"cond_value_2":    {""},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		t.Fatalf("decodeSteps: %v", err)
	}
	var saved domain.Step
	for _, s := range steps {
		if s.StepID == "in_app-1" {
			saved = s
		}
	}
	var groups []filter.Group
	if err := json.Unmarshal(saved.Filters, &groups); err != nil {
		t.Fatalf("step.Filters is not []filter.Group: %v (raw: %s)", err, saved.Filters)
	}
	if len(groups) != 1 || len(groups[0].Conditions) != 2 {
		t.Fatalf("want one group with 2 dense conditions after middle-remove, got %+v", groups)
	}
	got := groups[0].Conditions
	if got[0].Field != "data.a" || got[0].Operator != filter.OpEqual {
		t.Errorf("row 0 = %+v, want data.a/EQUAL", got[0])
	}
	if got[1].Field != "data.c" || got[1].Operator != filter.OpNotEqual {
		t.Errorf("row 1 = %+v, want data.c/NOT_EQUAL (the row shifted up, b removed)", got[1])
	}
}

// TestWorkflowStepSave_BlankRowBetweenFilledSkipped proves a blank Field row
// sandwiched between two filled rows is skipped, leaving two conditions saved.
func TestWorkflowStepSave_BlankRowBetweenFilledSkipped(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":      {csrf},
		"subject":         {"Hi"},
		"body":            {"Body"},
		"cond_combinator": {"AND"},
		"cond_on_0":       {"payload"},
		"cond_field_0":    {"data.a"},
		"cond_operator_0": {"EQUAL"},
		"cond_value_0":    {"x"},
		"cond_on_1":       {"payload"},
		"cond_field_1":    {""}, // blank row in the middle
		"cond_operator_1": {"EQUAL"},
		"cond_value_1":    {""},
		"cond_on_2":       {"payload"},
		"cond_field_2":    {"data.c"},
		"cond_operator_2": {"EQUAL"},
		"cond_value_2":    {"z"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		t.Fatalf("decodeSteps: %v", err)
	}
	var saved domain.Step
	for _, s := range steps {
		if s.StepID == "in_app-1" {
			saved = s
		}
	}
	var groups []filter.Group
	if err := json.Unmarshal(saved.Filters, &groups); err != nil {
		t.Fatalf("step.Filters is not []filter.Group: %v (raw: %s)", err, saved.Filters)
	}
	if len(groups) != 1 || len(groups[0].Conditions) != 2 {
		t.Fatalf("want one group with 2 conditions (blank middle skipped), got %+v", groups)
	}
	if groups[0].Conditions[0].Field != "data.a" || groups[0].Conditions[1].Field != "data.c" {
		t.Errorf("filled rows wrong: %+v", groups[0].Conditions)
	}
}
