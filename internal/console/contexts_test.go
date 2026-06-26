// contexts_test.go proves the W4-C2 contexts list + create sheet + detail
// editor + two-step delete console pages. Pattern follows webhooks_test.go.
package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ─── page render ──────────────────────────────────────────────────────────────

func TestContexts_ListPageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/contexts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/contexts: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Contexts") {
		t.Errorf("page missing 'Contexts' heading")
	}
}

func TestContexts_CreateFormRendersOnNew(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// LIST page: slide-over is present in DOM but hidden (contextNewOpen=false).
	rr := tc.get(t, "/console/contexts", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/contexts: status=%d body=%s", rr.Code, rr.Body.String())
	}
	listBody := rr.Body.String()
	if !strings.Contains(listBody, `data-component="context-create"`) {
		t.Errorf("list page: slide-over marker data-component=\"context-create\" missing")
	}
	if !strings.Contains(listBody, `data-show="$contextNewOpen"`) {
		t.Errorf("list page: data-show=\"$contextNewOpen\" missing from slide-over")
	}
	if !strings.Contains(listBody, `data-on:click="$contextNewOpen = true"`) {
		t.Errorf("list page: + Add button missing data-on:click=\"$contextNewOpen = true\"")
	}
	if strings.Contains(listBody, `href="/console/contexts/new"`) {
		t.Errorf("list page: must not contain href link to /console/contexts/new (causes layout shift)")
	}

	// /new route: seeds contextNewOpen=true so the slide-over is open on arrival.
	rr2 := tc.get(t, "/console/contexts/new", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET /console/contexts/new: status=%d", rr2.Code)
	}
	newBody := rr2.Body.String()
	if !strings.Contains(newBody, "context-create-form") && !strings.Contains(newBody, `data-component="context-create"`) {
		t.Errorf("create form not rendered on /console/contexts/new")
	}
	// The data-signals attribute JSON is HTML-escaped (" -> &#34;).
	if !strings.Contains(newBody, `&#34;contextNewOpen&#34;:true`) {
		t.Errorf("/new: contextNewOpen signal not seeded true")
	}
}

func TestContexts_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/contexts", cookie)
	if !strings.Contains(rr.Body.String(), `href="/console/contexts"`) {
		t.Errorf("contexts nav link missing")
	}
}

// ─── mutations ────────────────────────────────────────────────────────────────

func TestContexts_CreatePostRedirects303(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/contexts",
		url.Values{
			"type":       {"tenant"},
			"id":         {"acme"},
			"data":       {`{"plan":"enterprise"}`},
			"csrf_token": {csrf},
		}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /console/contexts: status=%d body=%s, want 303", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/console/contexts/tenant/acme") {
		t.Errorf("redirect location = %q, want /console/contexts/tenant/acme", loc)
	}
}

func TestContexts_InvalidJSONDoesNot500(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/contexts",
		url.Values{
			"type":       {"tenant"},
			"id":         {"bad"},
			"data":       {`{not valid json}`},
			"csrf_token": {csrf},
		}, cookie)
	if rr.Code == http.StatusInternalServerError {
		t.Fatalf("invalid JSON caused 500: %s", rr.Body.String())
	}
}

func TestContexts_CreateInvalidJSONShowsBanner(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// POST invalid JSON → should 303 to /console/contexts/new?dataError=...
	rr := tc.postForm(t, "/console/contexts",
		url.Values{
			"type":       {"tenant"},
			"id":         {"bad"},
			"data":       {`{not valid json}`},
			"csrf_token": {csrf},
		}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid JSON: want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "dataError") {
		t.Fatalf("redirect location %q missing dataError param", loc)
	}

	// Following the redirect must render the error banner.
	rr2 := tc.get(t, loc, cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", loc, rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "Invalid JSON") {
		t.Errorf("error banner missing from page after invalid JSON create; body:\n%s", rr2.Body.String())
	}
}

func TestContexts_CreateConflictShowsBanner(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Create once successfully.
	rr := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"dup"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("first create: status=%d", rr.Code)
	}

	// Create again with same type/id → should 303 to /console/contexts/new?conflict=1
	rr2 := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"dup"}, "csrf_token": {csrf}},
		cookie)
	if rr2.Code != http.StatusSeeOther {
		t.Fatalf("duplicate create: want 303, got %d body=%s", rr2.Code, rr2.Body.String())
	}
	loc := rr2.Header().Get("Location")
	if !strings.Contains(loc, "conflict") {
		t.Fatalf("redirect location %q missing conflict param", loc)
	}

	// Following the redirect must render the conflict banner.
	rr3 := tc.get(t, loc, cookie)
	if rr3.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", loc, rr3.Code, rr3.Body.String())
	}
	if !strings.Contains(rr3.Body.String(), "already exists") {
		t.Errorf("conflict banner missing from page; body:\n%s", rr3.Body.String())
	}
}

func TestContexts_UpdateInvalidJSONShowsBanner(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Seed a context to update.
	rr := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"upd-bad"}, "data": {`{"v":1}`}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d", rr.Code)
	}

	// POST invalid JSON to the update endpoint → 303 to detail page with ?dataError=...
	rr2 := tc.postForm(t, "/console/api/contexts/tenant/upd-bad",
		url.Values{"data": {`{not json}`}, "csrf_token": {csrf}},
		cookie)
	if rr2.Code != http.StatusSeeOther {
		t.Fatalf("invalid JSON update: want 303, got %d body=%s", rr2.Code, rr2.Body.String())
	}
	loc := rr2.Header().Get("Location")
	if !strings.Contains(loc, "dataError") {
		t.Fatalf("redirect location %q missing dataError param", loc)
	}

	// Following the redirect must render the error banner.
	rr3 := tc.get(t, loc, cookie)
	if rr3.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", loc, rr3.Code, rr3.Body.String())
	}
	if !strings.Contains(rr3.Body.String(), "Invalid JSON") {
		t.Errorf("error banner missing from detail page after invalid JSON update; body:\n%s", rr3.Body.String())
	}
}

func TestContexts_ReadOnlyBlocksMutation(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"ro"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only POST: status=%d, want 403", rr.Code)
	}
}

func TestContexts_DetailEditorPersists(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Create via the POST form.
	rr := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"persist-test"}, "data": {`{"v":1}`}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d", rr.Code)
	}

	// Update it.
	rr2 := tc.postForm(t, "/console/api/contexts/tenant/persist-test",
		url.Values{"data": {`{"v":2}`}, "csrf_token": {csrf}},
		cookie)
	if rr2.Code != http.StatusSeeOther {
		t.Fatalf("update: status=%d body=%s", rr2.Code, rr2.Body.String())
	}

	// Detail page must show updated data.
	rr3 := tc.get(t, "/console/contexts/tenant/persist-test", cookie)
	if rr3.Code != http.StatusOK {
		t.Fatalf("detail after update: status=%d", rr3.Code)
	}
	// The editor renders DataPretty inside a <textarea>, so quotes are
	// HTML-escaped (&#34;). Assert on the escaped form the browser receives.
	body := rr3.Body.String()
	if !strings.Contains(body, "&#34;v&#34;: 2") && !strings.Contains(body, `"v": 2`) {
		t.Errorf("updated data not visible in detail page: %s", body)
	}
}

func TestContexts_TwoStepDeleteConfirm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// Create.
	rr := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"to-delete"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d", rr.Code)
	}

	// First step: GET detail?confirmDelete=true renders confirm UI without deleting.
	rr2 := tc.get(t, "/console/contexts/tenant/to-delete?confirmDelete=true", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("confirm page: status=%d", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "Confirm delete") {
		t.Errorf("confirm button not in page: %s", rr2.Body.String())
	}

	// Second step: POST delete.
	rr3 := tc.postForm(t, "/console/api/contexts/tenant/to-delete/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr3.Code != http.StatusSeeOther {
		t.Fatalf("delete: status=%d body=%s", rr3.Code, rr3.Body.String())
	}
}

func TestContexts_FragmentEndpointRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/contexts-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContexts_NonDatastarPOSTRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/contexts",
		url.Values{"type": {"tenant"}, "id": {"redirect-test"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST not 303: status=%d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("POST returned SSE: Content-Type=%q", ct)
	}
}

// ─── scope isolation ──────────────────────────────────────────────────────────

func TestContexts_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	// Seed one context in the logged-in scope and one in altScope.
	if _, err := tc.deps.Contexts.Create(testBackground(), tc.scope, "tenant", "mine", nil); err != nil {
		t.Fatalf("seed own context: %v", err)
	}
	if _, err := tc.deps.Contexts.Create(testBackground(), altScope, "tenant", "theirs", nil); err != nil {
		t.Fatalf("seed alt context: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/contexts-list", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "mine") {
		t.Errorf("contexts list missing own context 'mine'")
	}
	if strings.Contains(body, "theirs") {
		t.Errorf("contexts list leaked alt-scope context 'theirs'")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

func TestContexts_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/contexts"},
		{http.MethodGet, "/console/contexts/new"},
		{http.MethodGet, "/console/contexts/tenant/x"},
		{http.MethodGet, "/console/api/fragments/contexts-list"},
		{http.MethodPost, "/console/contexts"},
		{http.MethodPost, "/console/api/contexts/tenant/x"},
		{http.MethodPost, "/console/api/contexts/tenant/x/delete"},
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
