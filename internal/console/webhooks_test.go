// webhooks_test.go proves the W2-D2 webhook endpoint list + CRUD console
// pages. Pattern follows topics_test.go.
//
// Test index:
//
//	TestWebhooks_PageRenderWithSeededData         — seeded endpoint URL appears in page
//	TestWebhooks_PageEmptyState                   — empty → "No webhook endpoints"
//	TestWebhooks_UnauthenticatedRedirectsToLogin  — no cookie → 303 to /console/login
//	TestWebhooks_NavEntryIsLive                   — nav link present + is-active class
//	TestFragment_WebhooksList_SSEFraming          — fragment GET has SSE event marker
//	TestFragment_WebhooksList_SeededRows          — fragment shows seeded URL
//	TestWebhooks_NewRendersCreateForm             — /new shows slide-over + csrf; /webhooks does NOT
//	TestWebhooks_ConfirmDeleteState               — fragment ?confirmDelete=<id> renders confirm row
//	TestWebhooks_Create_ReadOnlyBlocked           — POST create → 403 when ReadOnly
//	TestWebhooks_Toggle_ReadOnlyBlocked           — POST toggle → 403
//	TestWebhooks_Delete_ReadOnlyBlocked           — POST delete → 403
//	TestWebhooks_Create_Redirect                  — create → 303 to /console/webhooks
//	TestWebhooks_Toggle_ChangesActive             — toggle flips active; 303 back to list
//	TestWebhooks_Delete_RemovesEndpoint           — delete → 303; endpoint gone from list
//	TestWebhooks_Mutations_NonDatastarPOSTRedirects — native form POSTs get 303, never SSE
//	TestWebhooks_ScopeIsolation                   — endpoint in altScope not visible
//	TestWebhooks_RouteAuth                        — all new routes → 303 /console/login without session
package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// ─── seed helpers ─────────────────────────────────────────────────────────────

// seedWebhook creates a webhook endpoint in tc.scope via the management
// service.
func (tc *testConsole) seedWebhook(t *testing.T, rawURL string, active bool) domain.WebhookEndpoint {
	t.Helper()
	ep, err := tc.deps.Webhooks.Create(testBackground(), tc.scope, rawURL, active)
	if err != nil {
		t.Fatalf("seed webhook %q: %v", rawURL, err)
	}
	return ep.Endpoint
}

// ─── page render ──────────────────────────────────────────────────────────────

func TestWebhooks_PageRenderWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWebhook(t, "https://example.com/hooks/seeded", true)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/webhooks", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/webhooks: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "https://example.com/hooks/seeded") {
		t.Errorf("webhooks page missing seeded endpoint URL")
	}
}

func TestWebhooks_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/webhooks", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/webhooks empty: status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "No webhook endpoints") {
		t.Errorf("empty webhooks page missing empty-state copy")
	}
}

func TestWebhooks_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/webhooks", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/webhooks: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

func TestWebhooks_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/webhooks", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/webhooks: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/webhooks"`) {
		t.Errorf("webhooks page missing nav link")
	}
	if !strings.Contains(body, `is-active`) {
		t.Errorf("webhooks page nav link not marked as active")
	}
}

// ─── fragment ─────────────────────────────────────────────────────────────────

func TestFragment_WebhooksList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWebhook(t, "https://example.com/hooks/sse", true)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/webhooks-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("webhooks-list fragment: status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("webhooks-list fragment missing SSE event marker")
	}
}

func TestFragment_WebhooksList_SeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWebhook(t, "https://example.com/hooks/visible", true)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/webhooks-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("webhooks-list fragment: status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "https://example.com/hooks/visible") {
		t.Errorf("webhooks-list fragment missing seeded URL")
	}
}

// ─── create slide-over ────────────────────────────────────────────────────────

func TestWebhooks_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	// /new renders the create slide-over with the form + csrf token.
	rr := tc.get(t, "/console/webhooks/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/webhooks/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/console/webhooks"`) {
		t.Errorf("/new missing create form action")
	}
	if !strings.Contains(body, `data-component="webhook-create"`) {
		t.Errorf("/new missing webhook-create slide-over")
	}
	if !strings.Contains(body, csrf) {
		t.Errorf("/new create form missing CSRF token")
	}

	// The create slide-over is now a CLIENT-SIDE overlay: present on the list
	// page too, but hidden via data-show on the webhookNewOpen signal (seeded
	// false by the list handler, true by the /new handler).
	rrList := tc.get(t, "/console/webhooks", cookie)
	listBody := rrList.Body.String()

	if !strings.Contains(listBody, `data-component="webhook-create"`) {
		t.Error("list page should render the create slide-over as a hidden overlay")
	}
	if !strings.Contains(listBody, `data-show="$webhookNewOpen"`) {
		t.Error("create slide-over must be gated by data-show on the webhookNewOpen signal")
	}
	if !strings.Contains(listBody, `data-on:click="$webhookNewOpen = true"`) {
		t.Error(`"+ Add endpoint" must toggle the webhookNewOpen signal, not navigate`)
	}
	if strings.Contains(listBody, `href="/console/webhooks/new"`) {
		t.Error("list page must not contain a navigation link to /console/webhooks/new")
	}
}

func TestWebhooks_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	ep := tc.seedWebhook(t, "https://example.com/hooks/confirm", true)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/webhooks-list?confirmDelete="+ep.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirmDelete fragment missing confirm-row CTA")
	}
	if !strings.Contains(body, `action="/console/api/webhooks/`+ep.ID+`/delete"`) {
		t.Errorf("confirmDelete fragment missing delete POST form for endpoint")
	}
}

// ─── read-only gating ─────────────────────────────────────────────────────────

func TestWebhooks_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/webhooks",
		url.Values{"url": {"https://example.com/ro"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only create webhook: status = %d, want 403", rr.Code)
	}
}

func TestWebhooks_Toggle_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	ep := tc.seedWebhook(t, "https://example.com/ro-toggle", true)
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/api/webhooks/"+ep.ID+"/toggle", url.Values{}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only toggle webhook: status = %d, want 403", rr.Code)
	}
}

func TestWebhooks_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	ep := tc.seedWebhook(t, "https://example.com/ro-delete", true)
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/api/webhooks/"+ep.ID+"/delete", url.Values{}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only delete webhook: status = %d, want 403", rr.Code)
	}
}

// ─── mutations ────────────────────────────────────────────────────────────────

func TestWebhooks_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/webhooks",
		url.Values{"url": {"https://example.com/created"}, "active": {"1"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create webhook: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got, want := rr.Header().Get("Location"), "/console/webhooks"; got != want {
		t.Errorf("create webhook: Location = %q, want %q", got, want)
	}

	// The endpoint really landed in the store, active.
	eps, err := tc.deps.Webhooks.List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list after create: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("after create: %d endpoints, want 1", len(eps))
	}
	if eps[0].URL != "https://example.com/created" || !eps[0].Active {
		t.Errorf("created endpoint = %+v, want active https://example.com/created", eps[0])
	}
}

func TestWebhooks_Toggle_ChangesActive(t *testing.T) {
	tc := newTestConsole(t)
	ep := tc.seedWebhook(t, "https://example.com/toggle", false)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/webhooks/"+ep.ID+"/toggle",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("toggle webhook: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/webhooks" {
		t.Errorf("toggle redirect Location = %q, want /console/webhooks", got)
	}

	got, err := tc.deps.Webhooks.GetByID(testBackground(), tc.scope, ep.ID)
	if err != nil {
		t.Fatalf("get after toggle: %v", err)
	}
	if !got.Active {
		t.Errorf("toggle did not flip active: still inactive")
	}
}

func TestWebhooks_Delete_RemovesEndpoint(t *testing.T) {
	tc := newTestConsole(t)
	ep := tc.seedWebhook(t, "https://example.com/doomed", true)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/webhooks/"+ep.ID+"/delete",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete webhook: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/webhooks" {
		t.Errorf("delete redirect Location = %q, want /console/webhooks", got)
	}

	eps, err := tc.deps.Webhooks.List(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("after delete: %d endpoints, want 0", len(eps))
	}

	// The list page must no longer show the deleted endpoint.
	rr = tc.get(t, "/console/webhooks", cookie)
	if strings.Contains(rr.Body.String(), "https://example.com/doomed") {
		t.Errorf("deleted endpoint still present on the list page")
	}
}

// TestWebhooks_Mutations_NonDatastarPOSTRedirects is the regression gate for
// the raw-SSE-navigation defect: the tbody forms are NATIVE form POSTs, and a
// native form submission navigates to the response. Mutation handlers must
// therefore answer a plain 303 redirect — never a text/event-stream body,
// which the browser would render as raw `event:`/`data:` text.
func TestWebhooks_Mutations_NonDatastarPOSTRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	cases := []struct {
		name string
		path func() string
	}{
		{"update", func() string {
			ep := tc.seedWebhook(t, "https://example.com/mut-update", true)
			return "/console/api/webhooks/" + ep.ID
		}},
		{"toggle", func() string {
			ep := tc.seedWebhook(t, "https://example.com/mut-toggle", true)
			return "/console/api/webhooks/" + ep.ID + "/toggle"
		}},
		{"delete", func() string {
			ep := tc.seedWebhook(t, "https://example.com/mut-delete", true)
			return "/console/api/webhooks/" + ep.ID + "/delete"
		}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// Plain form POST: no Datastar header, no signals body —
			// exactly what a native <form method="POST"> submission sends.
			rr := tc.postForm(t, tt.path(),
				url.Values{"csrf_token": {csrf}, "url": {"https://example.com/updated"}},
				cookie)
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("%s: status = %d, want 303 (body: %s)",
					tt.name, rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Location"); got != "/console/webhooks" {
				t.Errorf("%s: Location = %q, want /console/webhooks", tt.name, got)
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

// ─── scope isolation ──────────────────────────────────────────────────────────

func TestWebhooks_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	tc.seedWebhook(t, "https://example.com/mine", true)
	if _, err := tc.deps.Webhooks.Create(testBackground(), altScope, "https://example.com/theirs", true); err != nil {
		t.Fatalf("seed alt webhook: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/webhooks-list", cookie)
	body := rr.Body.String()

	if !strings.Contains(body, "https://example.com/mine") {
		t.Errorf("webhooks list missing own endpoint 'mine'")
	}
	if strings.Contains(body, "https://example.com/theirs") {
		t.Errorf("webhooks list leaked alt-scope endpoint 'theirs'")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

// TestWebhooks_RouteAuth verifies all webhook routes redirect to login when
// called without a session cookie.
func TestWebhooks_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/webhooks"},
		{http.MethodGet, "/console/webhooks/new"},
		{http.MethodGet, "/console/api/fragments/webhooks-list"},
		{http.MethodPost, "/console/webhooks"},
		{http.MethodPost, "/console/api/webhooks/x"},
		{http.MethodPost, "/console/api/webhooks/x/toggle"},
		{http.MethodPost, "/console/api/webhooks/x/delete"},
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
				t.Errorf("%s %s: status = %d, want 303", rt.method, rt.path, rr.Code)
			}
			if got := rr.Header().Get("Location"); got != "/console/login" {
				t.Errorf("%s %s: Location = %q, want /console/login", rt.method, rt.path, got)
			}
		})
	}
}
