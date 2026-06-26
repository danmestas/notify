// variables_test.go proves the W4-V2 variables list + create sheet + detail
// editor + two-step delete console pages. Pattern follows contexts_test.go.
package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/envvars"
)

// ─── page render ──────────────────────────────────────────────────────────────

func TestVariables_ListPageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/variables", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/variables: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Variables") {
		t.Errorf("page missing 'Variables' heading")
	}
}

// TestVariables_NewRendersCreateForm pins the /new slide-over: the create
// form must actually render (form element + key input + CSRF token), not just
// return 200 with the plain list. The slide-over is a CLIENT-SIDE overlay:
// present on the list page too, but hidden via data-show on the variableNewOpen
// signal (seeded closed), so opening it causes NO layout shift.
func TestVariables_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/variables/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/variables/new: status=%d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, `data-component="variable-create"`) {
		t.Errorf("/new missing variable-create slide-over")
	}
	if !strings.Contains(body, `<form method="POST" action="/console/api/variables"`) {
		t.Errorf("/new missing create form posting to /console/api/variables")
	}
	if !strings.Contains(body, `name="key"`) {
		t.Errorf("/new create form missing key input")
	}
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("/new create form missing CSRF token")
	}

	// The list page must also render the slide-over as a hidden overlay —
	// toggled by the variableNewOpen signal, not by navigating to /new.
	listBody := tc.get(t, "/console/variables", cookie).Body.String()
	if !strings.Contains(listBody, `data-component="variable-create"`) {
		t.Error("list page should render the create slide-over as a hidden overlay")
	}
	if !strings.Contains(listBody, `data-show="$variableNewOpen"`) {
		t.Error("create slide-over must be gated by data-show on the variableNewOpen signal")
	}
	if !strings.Contains(listBody, `data-on:click="$variableNewOpen = true"`) {
		t.Error(`"+ Add variable" must toggle the variableNewOpen signal, not navigate`)
	}
	if strings.Contains(listBody, `href="/console/variables/new"`) {
		t.Error("the add control must not navigate to /new (causes layout shift)")
	}
}

func TestVariables_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/variables", cookie)
	if !strings.Contains(rr.Body.String(), `href="/console/variables"`) {
		t.Errorf("variables nav link missing")
	}
}

// ─── mutations ────────────────────────────────────────────────────────────────

func TestVariables_CreatePostRedirects303(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/variables",
		url.Values{
			"key":        {"MY_KEY"},
			"value":      {"my-value"},
			"is_secret":  {"on"},
			"csrf_token": {csrf},
		}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /console/api/variables: status=%d body=%s, want 303", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/console/variables/MY_KEY") {
		t.Errorf("redirect location = %q, want /console/variables/MY_KEY", loc)
	}
}

func TestVariables_ReadOnlyBlocksMutation(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/variables",
		url.Values{"key": {"RO_KEY"}, "value": {"val"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only POST: status=%d, want 403", rr.Code)
	}
}

func TestVariables_NonDatastarPostRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/variables",
		url.Values{"key": {"REDIR_KEY"}, "value": {"v"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST not 303: status=%d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("POST returned SSE: Content-Type=%q", ct)
	}
}

func TestVariables_TwoStepDelete(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Create.
	rr := tc.postForm(t, "/console/api/variables",
		url.Values{"key": {"TO_DEL"}, "value": {"v"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d", rr.Code)
	}

	// First step: GET detail?confirmDelete=true renders confirm UI.
	rr2 := tc.get(t, "/console/variables/TO_DEL?confirmDelete=true", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("confirm page: status=%d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "Confirm delete") {
		t.Errorf("confirm button not in page: %s", rr2.Body.String())
	}

	// Second step: POST delete.
	rr3 := tc.postForm(t, "/console/api/variables/TO_DEL/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr3.Code != http.StatusSeeOther {
		t.Fatalf("delete: status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	if loc := rr3.Header().Get("Location"); loc != "/console/variables" {
		t.Errorf("delete redirect = %q, want /console/variables", loc)
	}
}

func TestVariables_ValueNeverRawInMaskedView(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Seed a secret variable with a recognisable plaintext.
	plaintext := "supersecret123"
	rr := tc.postForm(t, "/console/api/variables",
		url.Values{
			"key":        {"SEC_VAR"},
			"value":      {plaintext},
			"is_secret":  {"on"},
			"csrf_token": {csrf},
		}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create secret: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// The variables list page must NOT contain the plaintext.
	rr2 := tc.get(t, "/console/variables", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET /console/variables: status=%d", rr2.Code)
	}
	body := rr2.Body.String()
	if strings.Contains(body, plaintext) {
		t.Errorf("list page leaked plaintext %q", plaintext)
	}
	if !strings.Contains(body, envvars.ValueMaskSentinel) {
		t.Errorf("list page missing mask sentinel")
	}
}

func TestVariables_RevealWorksOnDetailPage(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	plaintext := "reveal-me-now"
	rr := tc.postForm(t, "/console/api/variables",
		url.Values{
			"key":        {"REVEAL_VAR"},
			"value":      {plaintext},
			"is_secret":  {"on"},
			"csrf_token": {csrf},
		}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d", rr.Code)
	}

	// Without reveal — masked.
	rr2 := tc.get(t, "/console/variables/REVEAL_VAR", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("detail (masked): status=%d", rr2.Code)
	}
	if strings.Contains(rr2.Body.String(), plaintext) {
		t.Errorf("detail page leaked plaintext without reveal")
	}

	// With reveal=1 — plaintext.
	rr3 := tc.get(t, "/console/variables/REVEAL_VAR?reveal=1", cookie)
	if rr3.Code != http.StatusOK {
		t.Fatalf("detail (reveal): status=%d", rr3.Code)
	}
	if !strings.Contains(rr3.Body.String(), plaintext) {
		t.Errorf("detail page missing plaintext with reveal=1")
	}
}

func TestVariables_FragmentEndpointRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/variables-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// ─── scope isolation ──────────────────────────────────────────────────────────

func TestVariables_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	// Seed one variable in the logged-in scope and one in altScope.
	if _, err := tc.deps.EnvVars.Set(testBackground(), tc.scope, "MINE", true, "mine-val"); err != nil {
		t.Fatalf("seed own variable: %v", err)
	}
	if _, err := tc.deps.EnvVars.Set(testBackground(), altScope, "THEIRS", true, "their-val"); err != nil {
		t.Fatalf("seed alt variable: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/variables-list", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "MINE") {
		t.Errorf("variables list missing own variable 'MINE'")
	}
	if strings.Contains(body, "THEIRS") {
		t.Errorf("variables list leaked alt-scope variable 'THEIRS'")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

func TestVariables_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/variables"},
		{http.MethodGet, "/console/variables/new"},
		{http.MethodGet, "/console/variables/SOME_KEY"},
		{http.MethodGet, "/console/api/fragments/variables-list"},
		{http.MethodPost, "/console/api/variables"},
		{http.MethodPost, "/console/api/variables/SOME_KEY"},
		{http.MethodPost, "/console/api/variables/SOME_KEY/delete"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			var code int
			if rt.method == http.MethodGet {
				code = tc.get(t, rt.path, nil).Code
			} else {
				code = tc.postForm(t, rt.path, url.Values{}, nil).Code
			}
			if code != http.StatusSeeOther {
				t.Errorf("%s %s: status=%d, want 303", rt.method, rt.path, code)
			}
		})
	}
}
