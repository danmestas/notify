package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// ─── seed helper ─────────────────────────────────────────────────────────────

func (tc *testConsole) seedDomain(t *testing.T, name string) domain.InboundDomain {
	t.Helper()
	d, err := tc.deps.InboundDomains.Create(testBackground(), tc.scope, name)
	if err != nil {
		t.Fatalf("seed domain %q: %v", name, err)
	}
	return d
}

// ─── page render ─────────────────────────────────────────────────────────────

func TestDomains_ListPageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/domains", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/domains: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Inbound Domains") {
		t.Errorf("page missing 'Inbound Domains' heading")
	}
}

func TestDomains_ListShowsSeededDomain(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDomain(t, "seeded.com")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/domains", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/domains: status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "seeded.com") {
		t.Errorf("list page missing seeded domain name")
	}
}

func TestDomains_StatusChip_Pending(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDomain(t, "pending.com")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/domains-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Pending") {
		t.Errorf("fragment missing Pending status chip")
	}
}

func TestDomains_CreateFormRendersOnNew(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// LIST page: slide-over must be present in DOM (not gated by ShowCreate) but
	// hidden — driven by $domainNewOpen signal, which is false on the list route.
	listRR := tc.get(t, "/console/domains", cookie)
	if listRR.Code != http.StatusOK {
		t.Fatalf("GET /console/domains: status=%d body=%s", listRR.Code, listRR.Body.String())
	}
	listBody := listRR.Body.String()
	// Slide-over container must be in DOM with its data-component marker.
	if !strings.Contains(listBody, `data-component="domain-create"`) {
		t.Errorf("list page missing data-component=\"domain-create\" on slideover")
	}
	// Slide-over must carry data-show so Datastar controls visibility.
	if !strings.Contains(listBody, `data-show="$domainNewOpen"`) {
		t.Errorf("list page missing data-show=\"$domainNewOpen\" on slideover")
	}
	// Add button must be a button that sets the signal, not a hard nav link.
	if !strings.Contains(listBody, `data-on:click="$domainNewOpen = true"`) {
		t.Errorf("list page missing data-on:click=\"$domainNewOpen = true\" on Add button")
	}
	// No hard link to /new — that would cause a layout shift.
	if strings.Contains(listBody, `href="/console/domains/new"`) {
		t.Errorf("list page must not contain href=\"/console/domains/new\" (causes layout shift)")
	}

	// /new route: must render identically to the list page but with
	// domainNewOpen seeded true so the slide-over opens immediately.
	newRR := tc.get(t, "/console/domains/new", cookie)
	if newRR.Code != http.StatusOK {
		t.Fatalf("GET /console/domains/new: status=%d", newRR.Code)
	}
	newBody := newRR.Body.String()
	if !strings.Contains(newBody, `data-component="domain-create"`) {
		t.Errorf("/new missing data-component=\"domain-create\" on slideover")
	}
	if !strings.Contains(newBody, `data-show="$domainNewOpen"`) {
		t.Errorf("/new missing data-show=\"$domainNewOpen\" on slideover")
	}
	// Signal must be seeded true so the slide-over opens on page load.
	if !strings.Contains(newBody, `&#34;domainNewOpen&#34;:true`) {
		t.Errorf("/new missing domainNewOpen:true in page signals")
	}
}

func TestDomains_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/domains", cookie)
	if !strings.Contains(rr.Body.String(), `href="/console/domains"`) {
		t.Errorf("domains nav link missing")
	}
}

// ─── mutations ───────────────────────────────────────────────────────────────

func TestDomains_CreateRedirects303(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/domains",
		url.Values{"name": {"create-test.com"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST: status=%d body=%s, want 303", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "create-test.com") {
		t.Errorf("redirect location = %q, want /console/domains/create-test.com", loc)
	}
}

func TestDomains_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/domains",
		url.Values{"name": {"ro.com"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only POST: status=%d, want 403", rr.Code)
	}
}

func TestDomains_DetailPageRenders(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDomain(t, "detail.com")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/domains/detail.com", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET detail: status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "detail.com") {
		t.Errorf("detail page missing domain name")
	}
	if !strings.Contains(body, "DNS Records") {
		t.Errorf("detail page missing DNS Records section")
	}
}

func TestDomains_DetailShowsMXInstructions(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDomain(t, "mx.com")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/domains/mx.com", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "MX") {
		t.Errorf("detail page missing MX record type")
	}
}

func TestDomains_VerifyAction_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/api/domains/some.com/verify",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only verify: status=%d, want 403", rr.Code)
	}
}

func TestDomains_VerifyAction_RedirectsWith303(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDomain(t, "verify-test.com")
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/api/domains/verify-test.com/verify",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("verify: status=%d body=%s, want 303", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "verify-test.com") {
		t.Errorf("verify redirect = %q, missing domain name", loc)
	}
}

func TestDomains_NonDatastarPOSTRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	cases := []struct {
		name string
		path func() string
	}{
		{"create", func() string { return "/console/domains" }},
		{"verify", func() string {
			tc.seedDomain(t, "non-ds-verify.com")
			return "/console/api/domains/non-ds-verify.com/verify"
		}},
		{"delete", func() string {
			tc.seedDomain(t, "non-ds-delete.com")
			return "/console/api/domains/non-ds-delete.com/delete"
		}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rr := tc.postForm(t, tt.path(),
				url.Values{"name": {"x.com"}, "csrf_token": {csrf}},
				cookie)
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("%s: status=%d body=%s, want 303", tt.name, rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
				t.Errorf("%s: mutation returned SSE", tt.name)
			}
			if strings.Contains(rr.Body.String(), "datastar-patch-elements") {
				t.Errorf("%s: body contains datastar-patch-elements", tt.name)
			}
		})
	}
}

func TestDomains_TwoStepDelete(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDomain(t, "to-delete.com")
	cookie, csrf := tc.login(t)

	// Step 1: GET ?confirmDelete=true renders confirm UI.
	rr := tc.get(t, "/console/domains/to-delete.com?confirmDelete=true", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirmDelete GET: status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Confirm delete") {
		t.Errorf("confirm UI not rendered: %s", rr.Body.String())
	}

	// Step 2: POST delete.
	rr2 := tc.postForm(t, "/console/api/domains/to-delete.com/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr2.Code != http.StatusSeeOther {
		t.Fatalf("delete: status=%d body=%s", rr2.Code, rr2.Body.String())
	}

	// Verify domain is gone.
	rr3 := tc.get(t, "/console/domains/to-delete.com", cookie)
	if rr3.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", rr3.Code)
	}
}

func TestDomains_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/api/domains/some.com/delete",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only delete: status=%d, want 403", rr.Code)
	}
}

func TestDomains_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	tc.seedDomain(t, "mine.com")
	if _, err := tc.deps.InboundDomains.Create(testBackground(), altScope, "theirs.com"); err != nil {
		t.Fatalf("seed alt domain: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/domains-list", cookie)
	body := rr.Body.String()

	if !strings.Contains(body, "mine.com") {
		t.Errorf("fragment missing own domain 'mine.com'")
	}
	if strings.Contains(body, "theirs.com") {
		t.Errorf("fragment leaked alt-scope domain 'theirs.com'")
	}
}

func TestDomains_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/domains"},
		{http.MethodGet, "/console/domains/new"},
		{http.MethodGet, "/console/api/fragments/domains-list"},
		{http.MethodPost, "/console/domains"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			var rr *httptest.ResponseRecorder
			if rt.method == http.MethodGet {
				rr = tc.get(t, rt.path, nil)
			} else {
				rr = tc.postForm(t, rt.path, url.Values{}, nil)
			}
			if rr.Code != http.StatusSeeOther {
				t.Errorf("%s %s: status=%d, want 303", rt.method, rt.path, rr.Code)
			}
			if got := rr.Header().Get("Location"); got != "/console/login" {
				t.Errorf("%s %s: Location=%q, want /console/login", rt.method, rt.path, got)
			}
		})
	}
}

func TestConsoleTemplates_NoNativeConfirm_Domains(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	pages := []string{
		"/console/domains",
		"/console/domains/new",
	}
	for _, p := range pages {
		rr := tc.get(t, p, cookie)
		if strings.Contains(rr.Body.String(), "confirm(") || strings.Contains(rr.Body.String(), "alert(") {
			t.Errorf("%s: page contains native confirm() or alert()", p)
		}
	}
}
