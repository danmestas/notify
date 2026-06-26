// environments_test.go proves the W4-E1/E2 environments management console pages.
//
// Test index:
//
//	TestEnvironments_PageRendersWithSeededData        — seeded env appears in page
//	TestEnvironments_PageEmptyState                   — empty state absent when env exists
//	TestEnvironments_UnauthenticatedRedirectsToLogin  — no cookie → 303 to /console/login
//	TestEnvironments_NavEntryIsLive                   — nav link present + is-active
//	TestFragment_EnvironmentsList_SSEFraming          — fragment GET has SSE event marker
//	TestEnvironments_NewRendersCreateForm             — /new seeds envNewOpen=true; list renders hidden overlay
//	TestEnvironments_ConfirmDeleteState               — fragment ?confirmDelete shows confirm row
//	TestEnvironments_Create_ReadOnlyBlocked           — POST create → 403 when ReadOnly
//	TestEnvironments_Delete_ReadOnlyBlocked           — POST delete → 403
//	TestEnvironments_Create_Redirect                  — create → 303 to /console/settings/environments
//	TestEnvironments_Delete_LastEnvGuard              — cannot delete last environment
//	TestEnvironments_Delete_CurrentEnvGuard           — cannot delete current session environment
//	TestEnvironments_Delete_RemovesEnvironment        — delete non-current → 303; gone from list
//	TestEnvironments_NonDatastarPOSTRedirects         — native form POST gets 303, not SSE
//	TestEnvironments_RouteAuth                        — all routes → 303 /console/login without session
package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// ─── page render ──────────────────────────────────────────────────────────────

func TestEnvironments_PageRendersWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/environments", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/settings/environments: status = %d, want 200", rr.Code)
	}
	// Bootstrap creates "dev" environment; it must appear.
	body := rr.Body.String()
	if !strings.Contains(body, "dev") {
		t.Errorf("environments page missing bootstrap environment name")
	}
	// Must show the sidebar nav entry.
	if !strings.Contains(body, "Environments") {
		t.Errorf("environments page missing 'Environments' nav/heading")
	}
}

func TestEnvironments_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/settings/environments", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated: status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/console/login") {
		t.Errorf("redirect location = %q, want /console/login", loc)
	}
}

func TestEnvironments_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/settings/environments", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/settings/environments"`) {
		t.Error("nav link /console/settings/environments missing from page")
	}
	if !strings.Contains(body, "is-active") {
		t.Error("is-active class missing from active nav entry")
	}
}

func TestFragment_EnvironmentsList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/environments-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event:") {
		t.Error("SSE fragment missing 'event:' framing")
	}
	if !strings.Contains(body, "data:") {
		t.Error("SSE fragment missing 'data:' framing")
	}
}

func TestEnvironments_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// /new must render the slide-over (form + heading) and return 200.
	rr := tc.get(t, "/console/settings/environments/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-component="env-create"`) {
		t.Error("/new missing env-create slide-over")
	}
	if !strings.Contains(body, `<form method="POST" action="/console/settings/environments"`) {
		t.Error("/new missing create form posting to /console/settings/environments")
	}
	if !strings.Contains(body, `name="name"`) {
		t.Error("/new create form missing name input")
	}

	// The create slide-over is a CLIENT-SIDE overlay: present on the list page
	// too (hidden via data-show on the envNewOpen signal, seeded closed), so
	// opening it causes NO layout shift. The "+ Add environment" button toggles
	// the signal instead of navigating to /new.
	listBody := tc.get(t, "/console/settings/environments", cookie).Body.String()
	if !strings.Contains(listBody, `data-component="env-create"`) {
		t.Error("list page should render the create slide-over as a hidden overlay")
	}
	if !strings.Contains(listBody, `data-show="$envNewOpen"`) {
		t.Error("create slide-over must be gated by data-show on the envNewOpen signal")
	}
	if !strings.Contains(listBody, `data-on:click="$envNewOpen = true"`) {
		t.Error(`"+ Add environment" must toggle the envNewOpen signal, not navigate`)
	}
	if strings.Contains(listBody, `href="/console/settings/environments/new"`) {
		t.Error(`"+ Add environment" must not be a link to /new (causes layout shift)`)
	}
}

func TestEnvironments_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// Create a second environment so deleting one is allowed.
	_, _, scope2, err := tc.deps.Identity.CreateEnvironment(testBackground(), tc.scope.OrgID, "staging", "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	rr := tc.get(t, "/console/api/fragments/environments-list?confirmDelete="+scope2.EnvID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment confirmDelete: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirmDelete fragment missing 'Confirm delete' button")
	}
	if !strings.Contains(body, "cannot be undone") {
		t.Errorf("confirmDelete fragment missing warning text")
	}
}

// ─── mutation: create ─────────────────────────────────────────────────────────

func TestEnvironments_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/environments",
		url.Values{"name": {"staging"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("create read-only: status = %d, want 403", rr.Code)
	}
}

func TestEnvironments_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/settings/environments",
		url.Values{"name": {"staging"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/console/settings/environments" {
		t.Errorf("create redirect = %q, want /console/settings/environments", loc)
	}

	// Verify the environment appears in the list.
	rr2 := tc.get(t, "/console/settings/environments", cookie)
	if body := rr2.Body.String(); !strings.Contains(body, "staging") {
		t.Error("created environment 'staging' not visible in list")
	}
}

// ─── mutation: delete ─────────────────────────────────────────────────────────

func TestEnvironments_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/environments/some-id/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("delete read-only: status = %d, want 403", rr.Code)
	}
}

func TestEnvironments_Delete_LastEnvGuard(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Only one environment exists (the bootstrap "dev"). Delete must redirect
	// with an error, not actually delete.
	rr := tc.postForm(t, "/console/api/environments/"+tc.scope.EnvID+"/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	// It's the current env — should redirect (current guard fires first).
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete last/current env: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("delete last/current env: location %q missing error param", loc)
	}

	// Confirm the env still exists.
	envs, err := tc.deps.Identity.Store().Environments().List(testBackground(), tc.scope.OrgID)
	if err != nil {
		t.Fatalf("list after guard: %v", err)
	}
	if len(envs) != 1 {
		t.Errorf("env count after guard delete = %d, want 1", len(envs))
	}
}

func TestEnvironments_Delete_CurrentEnvGuard(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Create a second environment so the "last env" guard doesn't fire first.
	_, _, _, err := tc.deps.Identity.CreateEnvironment(testBackground(), tc.scope.OrgID, "staging", "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// Try to delete the current session environment.
	rr := tc.postForm(t, "/console/api/environments/"+tc.scope.EnvID+"/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete current env: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error=current") {
		t.Errorf("delete current env: location %q missing error=current", loc)
	}
}

func TestEnvironments_Delete_RemovesEnvironment(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Create a second environment to delete (non-current).
	_, _, scope2, err := tc.deps.Identity.CreateEnvironment(testBackground(), tc.scope.OrgID, "staging", "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	rr := tc.postForm(t, "/console/api/environments/"+scope2.EnvID+"/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303", rr.Code)
	}

	envs, err := tc.deps.Identity.Store().Environments().List(testBackground(), tc.scope.OrgID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(envs) != 1 {
		t.Errorf("env count after delete = %d, want 1", len(envs))
	}
	if envs[0].ID == scope2.EnvID {
		t.Error("deleted environment still present in list")
	}
}

func TestEnvironments_NonDatastarPOSTRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Native form POST to create must return 303, never SSE.
	rr := tc.postForm(t, "/console/settings/environments",
		url.Values{"name": {"e2e-test"}, "csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("create POST: status = %d, want 303", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		t.Errorf("create POST: content-type is SSE, want redirect")
	}
	if body := rr.Body.String(); strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("create POST body contains 'datastar-patch-elements' — SSE leaked into native form response")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

func TestEnvironments_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	routes := []string{
		"/console/settings/environments",
		"/console/settings/environments/new",
		"/console/api/fragments/environments-list",
	}
	for _, path := range routes {
		rr := tc.get(t, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("unauthenticated %s: status = %d, want 303", path, rr.Code)
		}
		if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/console/login") {
			t.Errorf("%s redirect = %q, want /console/login", path, loc)
		}
	}
}
