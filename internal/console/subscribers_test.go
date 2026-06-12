// subscribers_test.go proves the W2-C3 subscriber list + detail (and W2-C5
// activity tab) console pages. Pattern follows integrations_test.go:
// production services over a temp SQLite store, full login/session stack,
// white-box template assertions.
//
// Test index:
//
//	TestSubscribers_PageRenderWithSeededData         — page renders rows from store
//	TestSubscribers_PageEmptyState                   — empty store → empty-state copy
//	TestSubscribers_UnauthenticatedRedirectsToLogin  — auth gate on list page
//	TestSubscribers_NavEntryIsLive                   — nav link visible + active
//	TestFragment_SubscribersList_SSEFraming          — fragment has SSE event marker
//	TestFragment_SubscribersList_SeededRows          — fragment shows seeded subscriber
//	TestFragment_SubscribersList_FilterByEmail       — email filter narrows results
//	TestFragment_SubscribersList_FilterByName        — name filter narrows results
//	TestFragment_SubscribersList_SoftDeletedExcluded — soft-deleted rows not in list
//	TestSubscribers_DetailPage_Overview              — detail page overview tab
//	TestSubscribers_DetailPage_ActivityTab           — activity tab renders notifications
//	TestSubscribers_DetailPage_PreferencesTab        — preferences stub renders
//	TestSubscribers_DetailPage_NotFound              — unknown id → 404
//	TestSubscribers_Create_ReadOnlyBlocked           — POST create → 403 on read-only
//	TestSubscribers_Update_ReadOnlyBlocked           — POST update → 403 on read-only
//	TestSubscribers_Delete_ReadOnlyBlocked           — POST delete → 403 on read-only
//	TestSubscribers_Create_Redirect                  — successful create → redirect to list
//	TestSubscribers_Delete_Redirect                  — successful delete → redirect to list
//	TestSubscribers_ScopeIsolation                   — alt-scope rows not visible
//	TestSubscribers_RouteAuth                        — all new routes behind session gate
package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
)

// ─── seed helpers ─────────────────────────────────────────────────────────────

// seedSubscriber upserts a subscriber into tc.scope via the audience service.
func (tc *testConsole) seedSubscriber(t *testing.T, in audience.SubscriberInput) domain.Subscriber {
	t.Helper()
	sub, err := tc.deps.Audience.Upsert(testBackground(), tc.scope, in)
	if err != nil {
		t.Fatalf("seed subscriber: %v", err)
	}
	return sub
}

// seedSubscriberInScope upserts into an explicit scope (for scope-isolation).
func (tc *testConsole) seedSubscriberInScope(t *testing.T, scope domain.Scope, in audience.SubscriberInput) domain.Subscriber {
	t.Helper()
	sub, err := tc.deps.Audience.Upsert(testBackground(), scope, in)
	if err != nil {
		t.Fatalf("seed subscriber in scope: %v", err)
	}
	return sub
}

// ─── page render ──────────────────────────────────────────────────────────────

func TestSubscribers_PageRenderWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "usr-001",
		Email:        "alice@example.com",
		FirstName:    "Alice",
		LastName:     "Smith",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	if !strings.Contains(body, "Subscribers") {
		t.Errorf("subscribers page missing title")
	}
	if !strings.Contains(body, "usr-001") {
		t.Errorf("subscribers page missing seeded subscriber ID")
	}
	if !strings.Contains(body, "alice@example.com") {
		t.Errorf("subscribers page missing seeded email")
	}
}

func TestSubscribers_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers empty: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No subscribers") {
		t.Errorf("empty subscribers page missing empty-state copy (body snippet: %s)", body[:min(300, len(body))])
	}
}

func TestSubscribers_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/subscribers", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/subscribers: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

func TestSubscribers_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/subscribers"`) {
		t.Errorf("subscribers page missing nav link")
	}
	if !strings.Contains(body, `is-active`) {
		t.Errorf("subscribers page nav link not marked as active")
	}
}

// ─── list fragment ────────────────────────────────────────────────────────────

func TestFragment_SubscribersList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "sse-sub-001",
		Email:        "sse@example.com",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/subscribers-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("subscribers-list fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("subscribers-list fragment missing SSE event marker")
	}
}

func TestFragment_SubscribersList_SeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "visible-sub",
		Email:        "visible@example.com",
		FirstName:    "Visible",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/subscribers-list", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "visible-sub") {
		t.Errorf("subscribers-list fragment missing seeded subscriber ID")
	}
	if !strings.Contains(body, "visible@example.com") {
		t.Errorf("subscribers-list fragment missing seeded email")
	}
}

func TestFragment_SubscribersList_FilterByEmail(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "filter-email-a",
		Email:        "alpha@example.com",
	})
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "filter-email-b",
		Email:        "beta@example.com",
	})
	cookie, _ := tc.login(t)

	// Filter for "alpha" — should see alpha but not beta.
	rr := tc.getWithQuery(t, "/console/api/fragments/subscribers-list",
		url.Values{"subscribersEmail": {"alpha"}}.Encode(), cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "alpha@example.com") {
		t.Errorf("email-filtered list missing alpha@example.com")
	}
	if strings.Contains(body, "beta@example.com") {
		t.Errorf("email-filtered list leaked beta@example.com")
	}
}

func TestFragment_SubscribersList_FilterByName(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "name-alice",
		FirstName:    "Alice",
		LastName:     "Wonder",
	})
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "name-bob",
		FirstName:    "Bob",
		LastName:     "Builder",
	})
	cookie, _ := tc.login(t)

	// Filter by name "alice".
	rr := tc.getWithQuery(t, "/console/api/fragments/subscribers-list",
		url.Values{"subscribersName": {"alice"}}.Encode(), cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "name-alice") {
		t.Errorf("name-filtered list missing name-alice")
	}
	if strings.Contains(body, "name-bob") {
		t.Errorf("name-filtered list leaked name-bob")
	}
}

func TestFragment_SubscribersList_SoftDeletedExcluded(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "will-be-deleted",
		Email:        "del@example.com",
	})
	// Soft-delete via the audience service.
	if err := tc.deps.Audience.DeleteSubscriber(testBackground(), tc.scope, sub.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/subscribers-list", cookie)
	body := rr.Body.String()
	if strings.Contains(body, "will-be-deleted") {
		t.Errorf("subscribers list showed soft-deleted subscriber")
	}
}

// ─── detail page ──────────────────────────────────────────────────────────────

func TestSubscribers_DetailPage_Overview(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "detail-sub-001",
		Email:        "detail@example.com",
		FirstName:    "Detail",
		LastName:     "User",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers/"+sub.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET subscriber detail: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "detail-sub-001") {
		t.Errorf("detail page missing subscriber ID")
	}
	if !strings.Contains(body, "detail@example.com") {
		t.Errorf("detail page missing email")
	}
	if !strings.Contains(body, "Detail") {
		t.Errorf("detail page missing first name")
	}
}

func TestSubscribers_DetailPage_ActivityTab(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "activity-sub-001",
	})
	// Seed a notification for this subscriber.
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf-test",
		SubscriberID: sub.SubscriberID,
		RunID:        "run-abc123",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers/"+sub.ID+"?tab=activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET subscriber activity tab: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Activity tab content should be visible.
	if !strings.Contains(body, "Activity") {
		t.Errorf("activity tab missing 'Activity' heading")
	}
	// The seeded run ID should appear.
	if !strings.Contains(body, "run-abc123") {
		t.Errorf("activity tab missing seeded run ID 'run-abc123'")
	}
}

func TestSubscribers_DetailPage_PreferencesTab(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "prefs-sub-001",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers/"+sub.ID+"?tab=preferences", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET subscriber preferences tab: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Preferences") {
		t.Errorf("preferences tab missing heading")
	}
	// Stub copy.
	if !strings.Contains(body, "coming soon") {
		t.Errorf("preferences tab missing stub copy 'coming soon'")
	}
}

func TestSubscribers_DetailPage_NotFound(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers/nonexistent-ulid", cookie)
	if rr.Code != http.StatusNotFound {
		t.Errorf("detail page with bad id: status = %d, want 404", rr.Code)
	}
}

// ─── mutation handlers ────────────────────────────────────────────────────────

func TestSubscribers_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers",
		url.Values{"subscriberId": {"ro-create-001"}, "email": {"ro@example.com"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only create: status = %d, want 403", rr.Code)
	}
}

func TestSubscribers_Update_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "ro-update-sub",
		Email:        "ro-update@example.com",
	})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID,
		url.Values{"email": {"changed@example.com"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only update: status = %d, want 403", rr.Code)
	}
}

func TestSubscribers_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "ro-delete-sub",
	})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/delete",
		url.Values{},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only delete: status = %d, want 403", rr.Code)
	}
}

func TestSubscribers_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers",
		url.Values{
			"subscriberId": {"new-sub-001"},
			"email":        {"new@example.com"},
			"firstName":    {"New"},
			"csrf_token":   {csrf},
		},
		cookie)
	// Expect redirect to the new subscriber's detail page after create.
	if rr.Code != http.StatusSeeOther {
		t.Errorf("create subscriber: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	created, err := tc.deps.Audience.Get(testBackground(), tc.scope, "new-sub-001")
	if err != nil {
		t.Fatalf("created subscriber not persisted: %v", err)
	}
	if got, want := rr.Header().Get("Location"), "/console/subscribers/"+created.ID; got != want {
		t.Errorf("create subscriber: Location = %q, want %q", got, want)
	}
}

func TestSubscribers_Delete_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "to-delete-sub",
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/delete",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("delete subscriber: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/subscribers" {
		t.Errorf("delete subscriber: Location = %q, want /console/subscribers", got)
	}
}

// ─── create slide-over ────────────────────────────────────────────────────────

// TestSubscribers_NewRendersCreateForm pins the /new slide-over: the create
// form must actually render (form element + required subscriberId input +
// CSRF token), not just return 200 with the plain list.
func TestSubscribers_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/subscribers/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, `data-component="subscriber-create"`) {
		t.Errorf("/new missing subscriber-create slide-over")
	}
	if !strings.Contains(body, `<form method="POST" action="/console/subscribers"`) {
		t.Errorf("/new missing create form posting to /console/subscribers")
	}
	if !strings.Contains(body, `name="subscriberId"`) {
		t.Errorf("/new create form missing subscriberId input")
	}
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("/new create form missing CSRF token")
	}
	// Optional fields per spec-04 §2.2.
	for _, field := range []string{`name="email"`, `name="firstName"`, `name="lastName"`, `name="phone"`, `name="locale"`} {
		if !strings.Contains(body, field) {
			t.Errorf("/new create form missing optional field %s", field)
		}
	}

	// The plain list page must NOT render the slide-over.
	rr = tc.get(t, "/console/subscribers", cookie)
	if strings.Contains(rr.Body.String(), `data-component="subscriber-create"`) {
		t.Errorf("/console/subscribers rendered the create slide-over without /new")
	}
}

// ─── two-step inline confirm ──────────────────────────────────────────────────

// TestSubscribers_DetailDelete_TwoStepConfirm pins the chassis confirm idiom:
// the default detail page offers a link to the confirm state (no POST form,
// no native confirm()); ?confirmDelete=true renders Cancel + a real POST form.
func TestSubscribers_DetailDelete_TwoStepConfirm(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "confirm-sub-001",
	})
	cookie, csrf := tc.login(t)

	// Step 1: default state — confirm link, no delete POST form.
	rr := tc.get(t, "/console/subscribers/"+sub.ID, cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "confirmDelete=true") {
		t.Errorf("detail page missing confirmDelete link")
	}
	if strings.Contains(body, "/console/subscribers/"+sub.ID+"/delete") {
		t.Errorf("default detail page rendered the delete POST form before confirm")
	}
	if strings.Contains(body, "onsubmit") {
		t.Errorf("detail page uses native onsubmit confirm")
	}

	// Step 2: confirm state — Cancel + real POST form with CSRF.
	rr = tc.get(t, "/console/subscribers/"+sub.ID+"?confirmDelete=true", cookie)
	body = rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirm state missing 'Confirm delete' button")
	}
	if !strings.Contains(body, "Cancel") {
		t.Errorf("confirm state missing Cancel")
	}
	if !strings.Contains(body, `action="/console/subscribers/`+sub.ID+`/delete"`) {
		t.Errorf("confirm state missing delete POST form")
	}
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("confirm-delete form missing CSRF token")
	}
}

// TestFragment_SubscribersList_ConfirmDeleteState pins the fragment-level
// two-step confirm: ?confirmDelete=<id> renders that row in confirm state.
func TestFragment_SubscribersList_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "frag-confirm-sub",
	})
	cookie, _ := tc.login(t)

	// Plain fragment: no confirm form rendered.
	rr := tc.get(t, "/console/api/fragments/subscribers-list", cookie)
	body := rr.Body.String()
	if strings.Contains(body, "Confirm delete") {
		t.Errorf("plain fragment rendered confirm state without confirmDelete param")
	}

	// Confirm state for the row.
	rr = tc.getWithQuery(t, "/console/api/fragments/subscribers-list",
		url.Values{"confirmDelete": {sub.ID}}.Encode(), cookie)
	body = rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirm fragment missing 'Confirm delete' button")
	}
	if !strings.Contains(body, `action="/console/subscribers/`+sub.ID+`/delete"`) {
		t.Errorf("confirm fragment missing delete POST form for row")
	}
	if !strings.Contains(body, "Cancel") {
		t.Errorf("confirm fragment missing Cancel")
	}
}

// TestConsoleTemplates_NoNativeConfirm is the regression gate for the
// chassis idiom: no template may use a native confirm()/alert()/prompt()
// dialog — they are off-idiom and break headless verification.
func TestConsoleTemplates_NoNativeConfirm(t *testing.T) {
	entries := []string{"templates", "templates/partials"}
	for _, dir := range entries {
		files, err := templatesFS.ReadDir(dir)
		if err != nil {
			t.Fatalf("read embedded dir %s: %v", dir, err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			raw, err := templatesFS.ReadFile(dir + "/" + f.Name())
			if err != nil {
				t.Fatalf("read embedded %s/%s: %v", dir, f.Name(), err)
			}
			for _, banned := range []string{"onsubmit=", "confirm('", `confirm("`, "alert(", "prompt("} {
				if strings.Contains(string(raw), banned) {
					t.Errorf("%s/%s contains banned native-dialog hook %q", dir, f.Name(), banned)
				}
			}
		}
	}
}

// ─── scope isolation ──────────────────────────────────────────────────────────

func TestSubscribers_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "my-subscriber",
		Email:        "mine@example.com",
	})
	tc.seedSubscriberInScope(t, altScope, audience.SubscriberInput{
		SubscriberID: "their-subscriber",
		Email:        "theirs@example.com",
	})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/subscribers", cookie)
	body := rr.Body.String()

	if !strings.Contains(body, "my-subscriber") {
		t.Errorf("subscribers page missing own subscriber 'my-subscriber'")
	}
	if strings.Contains(body, "their-subscriber") {
		t.Errorf("subscribers page leaked alt-scope subscriber 'their-subscriber'")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

// TestSubscribers_RouteAuth verifies all subscribers routes redirect to login
// when called without a session cookie.
func TestSubscribers_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/subscribers"},
		{http.MethodGet, "/console/subscribers/new"},
		{http.MethodGet, "/console/subscribers/x"},
		{http.MethodGet, "/console/api/fragments/subscribers-list"},
		{http.MethodPost, "/console/subscribers"},
		{http.MethodPost, "/console/subscribers/x"},
		{http.MethodPost, "/console/subscribers/x/delete"},
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
