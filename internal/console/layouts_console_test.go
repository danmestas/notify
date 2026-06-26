// layouts_console_test.go proves the W4-L1 layouts list + create + editor
// console pages. Pattern follows webhooks_test.go.
//
// Test index:
//
//	TestLayouts_PageRenderWithSeededData          — seeded layout key appears in page
//	TestLayouts_PageEmptyState                    — empty → "No layouts yet"
//	TestLayouts_UnauthenticatedRedirectsToLogin   — no cookie → 303 to /console/login
//	TestLayouts_NavEntryIsLive                    — nav link present + is-active class
//	TestFragment_LayoutsList_SSEFraming           — fragment GET has SSE event marker
//	TestFragment_LayoutsList_SeededRows           — fragment shows seeded key
//	TestLayouts_NewRendersCreateForm              — /new shows slide-over; /layouts does NOT
//	TestLayouts_ListDoesNotRenderCreateForm       — list page omits the create slide-over
//	TestLayouts_ConfirmDeleteState                — fragment ?confirmDelete=key renders confirm row
//	TestLayouts_Create_ReadOnlyBlocked            — POST create → 403 when ReadOnly
//	TestLayouts_Save_ReadOnlyBlocked              — POST save → 403
//	TestLayouts_Delete_ReadOnlyBlocked            — POST delete → 403
//	TestLayouts_Create_Redirect                   — create → 303 to /console/layouts/{key}
//	TestLayouts_Save_Persists                     — save updates name; reload shows it
//	TestLayouts_Delete_Removes                    — delete → 303; key gone from list
//	TestLayouts_Mutations_NonDatastarPostRedirects — native form POSTs get 303, never SSE
//	TestLayouts_ScopeIsolation                    — layout in altScope not visible
//	TestConsoleLayouts_RouteAuth                  — all /console/layouts routes behind session gate
package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// ─── seed helper ──────────────────────────────────────────────────────────────

// seedLayout upserts a layout in tc.scope and returns the stored row.
func (tc *testConsole) seedLayout(t *testing.T, key, name, content string) domain.Layout {
	t.Helper()
	if err := tc.st.Layouts().Upsert(context.Background(), tc.scope, key, name, content); err != nil {
		t.Fatalf("seed layout %q: %v", key, err)
	}
	l, err := tc.st.Layouts().Get(context.Background(), tc.scope, key)
	if err != nil {
		t.Fatalf("get seeded layout %q: %v", key, err)
	}
	return l
}

// ─── page render ──────────────────────────────────────────────────────────────

func TestLayouts_PageRenderWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLayout(t, "welcome", "Welcome", "{{content}}")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/layouts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/layouts: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "welcome") {
		t.Errorf("layouts page missing seeded key 'welcome'")
	}
}

func TestLayouts_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/layouts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/layouts: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "No layouts yet") {
		t.Errorf("empty layouts page missing empty-state copy")
	}
}

func TestLayouts_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/layouts", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/layouts: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

func TestLayouts_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/layouts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/layouts: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/layouts"`) {
		t.Errorf("layouts page missing nav link")
	}
	if !strings.Contains(body, "is-active") {
		t.Errorf("layouts page nav link not marked as active")
	}
}

// ─── fragment ─────────────────────────────────────────────────────────────────

func TestFragment_LayoutsList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLayout(t, "sse", "SSE", "{{content}}")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/layouts-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("layouts-list fragment: status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("layouts-list fragment missing SSE event marker")
	}
}

func TestFragment_LayoutsList_SeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLayout(t, "visible-key", "Visible", "{{content}}")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/layouts-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("layouts-list fragment: status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "visible-key") {
		t.Errorf("layouts-list fragment missing seeded key")
	}
}

// ─── create slide-over ────────────────────────────────────────────────────────

func TestLayouts_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/layouts/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/layouts/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-component="layout-create"`) {
		t.Errorf("/new missing create slide-over component")
	}
	if !strings.Contains(body, `action="/console/api/layouts"`) {
		t.Errorf("/new missing create form action")
	}
	if !strings.Contains(body, csrf) {
		t.Errorf("/new create form missing csrf token")
	}
}

func TestLayouts_ListDoesNotRenderCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/layouts", cookie)
	if strings.Contains(rr.Body.String(), `data-component="layout-create"`) {
		t.Errorf("/console/layouts should not render the create slide-over")
	}
}

func TestLayouts_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	l := tc.seedLayout(t, "confirm-key", "Confirm", "{{content}}")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/layouts-list?confirmDelete="+l.Key, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirmDelete fragment missing confirm-row CTA")
	}
	if !strings.Contains(body, `action="/console/api/layouts/`+l.Key+`/delete"`) {
		t.Errorf("confirmDelete fragment missing delete POST form for layout")
	}
}

// ─── read-only gating ─────────────────────────────────────────────────────────

func TestLayouts_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts",
		url.Values{"key": {"ro"}, "name": {"RO"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only create layout: status = %d, want 403", rr.Code)
	}
}

func TestLayouts_Save_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	l := tc.seedLayout(t, "ro-save", "RO Save", "{{content}}")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts/"+l.Key+"/save",
		url.Values{"name": {"Changed"}, "content": {"{{content}}"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only save layout: status = %d, want 403", rr.Code)
	}
}

func TestLayouts_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	l := tc.seedLayout(t, "ro-delete", "RO Delete", "{{content}}")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts/"+l.Key+"/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only delete layout: status = %d, want 403", rr.Code)
	}
}

// ─── mutations ────────────────────────────────────────────────────────────────

func TestLayouts_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts",
		url.Values{"key": {"created-key"}, "name": {"Created"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create layout: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/layouts/created-key"; got != want {
		t.Errorf("create layout: Location = %q, want %q", got, want)
	}

	// The layout really landed in the store.
	l, err := tc.st.Layouts().Get(testBackground(), tc.scope, "created-key")
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if l.Name != "Created" {
		t.Errorf("created layout name = %q, want Created", l.Name)
	}
}

func TestLayouts_Save_Persists(t *testing.T) {
	tc := newTestConsole(t)
	l := tc.seedLayout(t, "save-key", "Original", "{{content}}")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts/"+l.Key+"/save",
		url.Values{"name": {"Updated Name"}, "content": {"<b>{{content}}</b>"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save layout: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/layouts/save-key"; got != want {
		t.Errorf("save layout: Location = %q, want %q", got, want)
	}

	// Reload the editor: the updated name is visible.
	rr2 := tc.get(t, "/console/layouts/save-key", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("reload editor: status = %d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "Updated Name") {
		t.Errorf("editor reload missing updated name")
	}
}

func TestLayouts_Delete_Removes(t *testing.T) {
	tc := newTestConsole(t)
	l := tc.seedLayout(t, "doomed-key", "Doomed", "{{content}}")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts/"+l.Key+"/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete layout: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/layouts"; got != want {
		t.Errorf("delete layout: Location = %q, want %q", got, want)
	}

	// Gone from the list fragment.
	rr2 := tc.get(t, "/console/api/fragments/layouts-list", cookie)
	if strings.Contains(rr2.Body.String(), "doomed-key") {
		t.Errorf("deleted layout still present in list fragment")
	}
}

func TestLayouts_Mutations_NonDatastarPostRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	cases := []struct {
		name string
		path func() string
		form url.Values
	}{
		{
			"create", func() string { return "/console/api/layouts" },
			url.Values{"key": {"mut-create"}, "name": {"MutCreate"}, "csrf_token": {csrf}},
		},
		{"save", func() string {
			l := tc.seedLayout(t, "mut-save", "MutSave", "{{content}}")
			return "/console/api/layouts/" + l.Key + "/save"
		}, url.Values{"name": {"MutSaved"}, "content": {"{{content}}"}, "csrf_token": {csrf}}},
		{"delete", func() string {
			l := tc.seedLayout(t, "mut-delete", "MutDelete", "{{content}}")
			return "/console/api/layouts/" + l.Key + "/delete"
		}, url.Values{"csrf_token": {csrf}}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rr := tc.postForm(t, tt.path(), tt.form, cookie)
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("%s: status = %d, want 303 (body: %s)", tt.name, rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
				t.Errorf("%s: mutation answered SSE (%q) to a native form POST", tt.name, ct)
			}
			if body := rr.Body.String(); strings.Contains(body, "datastar-patch-elements") {
				t.Errorf("%s: mutation response carries a raw SSE patch body", tt.name)
			}
		})
	}
}

func TestLayouts_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	tc.seedLayout(t, "mine", "Mine", "{{content}}")
	if _, err := tc.st.Layouts().Create(testBackground(), altScope, "theirs", "Theirs", "{{content}}"); err != nil {
		t.Fatalf("seed alt layout: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/layouts-list", cookie)
	body := rr.Body.String()

	if !strings.Contains(body, "mine") {
		t.Errorf("layouts list missing own layout 'mine'")
	}
	if strings.Contains(body, "theirs") {
		t.Errorf("layouts list leaked alt-scope layout 'theirs'")
	}
}

func TestConsoleLayouts_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/layouts"},
		{http.MethodGet, "/console/layouts/new"},
		{http.MethodGet, "/console/layouts/some-key"},
		{http.MethodPost, "/console/api/layouts"},
		{http.MethodPost, "/console/api/layouts/x/save"},
		{http.MethodPost, "/console/api/layouts/x/delete"},
		{http.MethodPost, "/console/api/layouts/x/default"},
		{http.MethodGet, "/console/api/fragments/layouts-list"},
		{http.MethodGet, "/console/api/fragments/layout-editor"},
	}
	for _, rt := range routes {
		resp := tc.get(t, rt.path, nil)
		if rt.method == http.MethodPost {
			resp = tc.postForm(t, rt.path, url.Values{}, nil)
		}
		if resp.Code != http.StatusSeeOther {
			t.Errorf("%s %s without session: status = %d, want 303", rt.method, rt.path, resp.Code)
			continue
		}
		if got := resp.Header().Get("Location"); got != "/console/login" {
			t.Errorf("%s %s: Location = %q, want /console/login", rt.method, rt.path, got)
		}
	}
}

// ─── set default ──────────────────────────────────────────────────────────────

// TestLayouts_SetDefault_FlipsDefault verifies that POST .../default marks the
// given layout as the sole default for the scope: a second call on a different
// key clears the first key's flag.
func TestLayouts_SetDefault_FlipsDefault(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLayout(t, "layout-a", "Layout A", "{{content}}")
	tc.seedLayout(t, "layout-b", "Layout B", "{{content}}")
	cookie, csrf := tc.login(t)

	// Set layout-a as default → 303 to /console/layouts.
	rr := tc.postForm(t, "/console/api/layouts/layout-a/default",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("set default layout-a: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/layouts"; got != want {
		t.Errorf("set default layout-a: Location = %q, want %q", got, want)
	}

	// Store agrees: layout-a is now default.
	la, err := tc.st.Layouts().Get(testBackground(), tc.scope, "layout-a")
	if err != nil {
		t.Fatalf("get layout-a: %v", err)
	}
	if !la.IsDefault {
		t.Errorf("layout-a IsDefault = false after set-default, want true")
	}

	// The list fragment renders the default badge on layout-a.
	rr2 := tc.get(t, "/console/api/fragments/layouts-list", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("list fragment after set-default: status = %d", rr2.Code)
	}
	body2 := rr2.Body.String()
	if !strings.Contains(body2, "chip-success") {
		t.Errorf("list fragment after set-default: missing default badge (chip-success)")
	}

	// Set layout-b as default: layout-a must lose its flag (only one per scope).
	rr3 := tc.postForm(t, "/console/api/layouts/layout-b/default",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr3.Code != http.StatusSeeOther {
		t.Fatalf("set default layout-b: status = %d, want 303", rr3.Code)
	}

	lb, err := tc.st.Layouts().Get(testBackground(), tc.scope, "layout-b")
	if err != nil {
		t.Fatalf("get layout-b: %v", err)
	}
	if !lb.IsDefault {
		t.Errorf("layout-b IsDefault = false after set-default, want true")
	}

	la2, err := tc.st.Layouts().Get(testBackground(), tc.scope, "layout-a")
	if err != nil {
		t.Fatalf("get layout-a post layout-b set-default: %v", err)
	}
	if la2.IsDefault {
		t.Errorf("layout-a IsDefault = true after layout-b set-default, want false (only one default per scope)")
	}
}

// TestLayouts_SetDefault_ReadOnlyBlocked verifies POST .../default → 403 in
// read-only mode.
func TestLayouts_SetDefault_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedLayout(t, "ro-default-key", "RO Default", "{{content}}")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts/ro-default-key/default",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only set default: status = %d, want 403", rr.Code)
	}
}

// TestLayouts_SetDefault_NonDatastarPostRedirects verifies the set-default POST
// returns a 303 (not SSE) — same contract as the other layout mutations.
func TestLayouts_SetDefault_NonDatastarPostRedirects(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedLayout(t, "nd-default", "ND Default", "{{content}}")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/layouts/nd-default/default",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("set default: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("set default answered SSE (%q) to a native form POST", ct)
	}
	if body := rr.Body.String(); strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("set default response carries a raw SSE patch body")
	}
}
