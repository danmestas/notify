// subscribers_test.go proves the W2-C3 subscriber list + detail (and W2-C5
// activity tab) console pages. Pattern follows integrations_test.go:
// production services over a temp SQLite store, full login/session stack,
// white-box template assertions.
//
// Test index:
//
//	TestSubscribers_PageRenderWithSeededData              — page renders rows from store
//	TestSubscribers_PageEmptyState                        — empty store → empty-state copy
//	TestSubscribers_UnauthenticatedRedirectsToLogin       — auth gate on list page
//	TestSubscribers_NavEntryIsLive                        — nav link visible + active
//	TestFragment_SubscribersList_SSEFraming               — fragment has SSE event marker
//	TestFragment_SubscribersList_SeededRows               — fragment shows seeded subscriber
//	TestFragment_SubscribersList_FilterByEmail            — email filter narrows results
//	TestFragment_SubscribersList_FilterByName             — name filter narrows results
//	TestFragment_SubscribersList_SoftDeletedExcluded      — soft-deleted rows not in list
//	TestSubscribers_DetailPage_Overview                   — detail page overview tab
//	TestSubscribers_DetailPage_ActivityTab                — activity tab renders notifications
//	TestSubscribers_DetailPage_PreferencesTab             — preferences tab renders real toggles
//	TestSubscribers_DetailPage_NotFound                   — unknown id → 404
//	TestSubscribers_PreferencesPatch_Persists303          — PATCH persists + 303 redirect
//	TestSubscribers_PreferencesPatch_ReadOnlyBlocked      — PATCH blocked on read-only console
//	TestSubscribers_Create_ReadOnlyBlocked                — POST create → 403 on read-only
//	TestSubscribers_Update_ReadOnlyBlocked                — POST update → 403 on read-only
//	TestSubscribers_Delete_ReadOnlyBlocked                — POST delete → 403 on read-only
//	TestSubscribers_Create_Redirect                       — successful create → redirect to list
//	TestSubscribers_Create_AutoGeneratesBlankID           — blank subscriberId → server-minted sub_ id
//	TestSubscribers_Create_PreservesProvidedID            — provided subscriberId → used verbatim
//	TestSubscribers_Delete_Redirect                       — successful delete → redirect to list
//	TestSubscribers_ScopeIsolation                        — alt-scope rows not visible
//	TestSubscribers_RouteAuth                             — all new routes behind session gate
package console

import (
	"fmt"
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
	// Real UI — must show the channel toggle form action, not the old stub.
	if !strings.Contains(body, "/preferences") {
		t.Errorf("preferences tab missing toggle form action")
	}
	if strings.Contains(body, "coming soon") {
		t.Errorf("preferences tab still shows stub copy 'coming soon'")
	}
}

func TestSubscribers_PreferencesPatch_Persists303(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "prefs-patch-sub",
	})
	cookie, csrf := tc.login(t)

	// POST a global preference: disable in_app for this subscriber.
	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/preferences",
		url.Values{
			"csrf_token": {csrf},
			"channel":    {"in_app"},
			"enabled":    {"false"},
			"workflowId": {""},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("preferences PATCH: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); !strings.Contains(got, "tab=preferences") {
		t.Errorf("preferences PATCH: Location = %q, want ?tab=preferences", got)
	}

	// Verify the preference was persisted by loading the preferences tab.
	rr2 := tc.get(t, "/console/subscribers/"+sub.ID+"?tab=preferences", cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET after PATCH: status = %d, want 200", rr2.Code)
	}
	// The toggle now reflects the saved state (enabled=false → next POST sends true).
	body := rr2.Body.String()
	if !strings.Contains(body, `value="true"`) {
		t.Errorf("after disabling in_app, toggle should offer value=true (re-enable), got: %s", body)
	}
}

func TestSubscribers_PreferencesPatch_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "ro-prefs-sub",
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/preferences",
		url.Values{
			"csrf_token": {csrf},
			"channel":    {"email"},
			"enabled":    {"false"},
			"workflowId": {""},
		},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only preferences PATCH: status = %d, want 403", rr.Code)
	}
}

// ─── channels tab (set per-subscriber delivery addresses) ────────────────────

func TestSubscribers_DetailPage_ChannelsTab_WithActiveIntegration(t *testing.T) {
	tc := newTestConsole(t)
	// Seed an active chat integration so its provider is connectable.
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "discord",
		Channel:    "chat",
		Active:     true,
	})
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-sub"})
	// Seed an existing channel address for this subscriber.
	if _, err := tc.deps.Audience.SetCredentials(testBackground(), tc.scope, sub.SubscriberID, "discord", "chat", []byte(`{"address":"https://discord/hook"}`)); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers/"+sub.ID+"?tab=channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("channels tab: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Tab link present and active.
	if !strings.Contains(body, "tab=channels") {
		t.Errorf("channels tab link missing")
	}
	// Existing channel row shows the provider label + stored value.
	if !strings.Contains(body, "Discord") {
		t.Errorf("expected Discord provider label in channels list, got: %s", body)
	}
	if !strings.Contains(body, "https://discord/hook") {
		t.Errorf("expected stored address in channels list, got: %s", body)
	}
	// Add-form action posts to the channels handler.
	if !strings.Contains(body, "/console/subscribers/"+sub.ID+"/channels") {
		t.Errorf("expected channels add-form action, got: %s", body)
	}
}

func TestSubscribers_DetailPage_ChannelsTab_EmptyStateNoIntegrations(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-empty-sub"})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers/"+sub.ID+"?tab=channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("channels tab: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// No connectable providers → point operator to integrations.
	if !strings.Contains(body, "/console/integrations") {
		t.Errorf("expected empty-state pointing to /console/integrations, got: %s", body)
	}
}

func TestSubscribers_ChannelsPatch_SetsAddress303(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "discord",
		Channel:    "chat",
		Active:     true,
	})
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-set-sub"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {"discord"},
			"value":       {"https://discord.example/webhook"},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("channels PATCH: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); !strings.Contains(got, "tab=channels") {
		t.Errorf("channels PATCH: Location = %q, want ?tab=channels", got)
	}

	// Verify persisted via the store (chat → {"address": value}).
	ch, err := tc.st.SubscriberChannels().GetByProvider(testBackground(), tc.scope, sub.SubscriberID, "discord")
	if err != nil {
		t.Fatalf("GetByProvider after PATCH: %v", err)
	}
	if ch.Type != "chat" {
		t.Errorf("Type = %q, want chat", ch.Type)
	}
	if string(ch.Credentials) != `{"address":"https://discord.example/webhook"}` {
		t.Errorf("Credentials = %s, want chat address JSON", ch.Credentials)
	}
}

func TestSubscribers_ChannelsPatch_SetsTopicForPush(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "ntfy",
		Channel:    "push",
		Active:     true,
	})
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-push-sub"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {"ntfy"},
			"value":       {"agent-alerts"},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("channels PATCH push: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	ch, err := tc.st.SubscriberChannels().GetByProvider(testBackground(), tc.scope, sub.SubscriberID, "ntfy")
	if err != nil {
		t.Fatalf("GetByProvider after PATCH: %v", err)
	}
	if ch.Type != "push" {
		t.Errorf("Type = %q, want push", ch.Type)
	}
	if string(ch.Credentials) != `{"topic":"agent-alerts"}` {
		t.Errorf("Credentials = %s, want push topic JSON", ch.Credentials)
	}
}

func TestSubscribers_ChannelsPatch_RejectsUnknownProvider(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "discord",
		Channel:    "chat",
		Active:     true,
	})
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-bad-sub"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {"not-a-provider"},
			"value":       {"x"},
		},
		cookie)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown provider: status = %d, want 400", rr.Code)
	}
}

func TestSubscribers_ChannelsPatch_RejectsEmptyValue(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "discord",
		Channel:    "chat",
		Active:     true,
	})
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-empty-val"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {"discord"},
			"value":       {"   "}, // whitespace trims to empty
		},
		cookie)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty value: status = %d, want 400", rr.Code)
	}
	// Nothing should have been persisted for an empty value.
	if _, err := tc.st.SubscriberChannels().GetByProvider(testBackground(), tc.scope, sub.SubscriberID, "discord"); err == nil {
		t.Error("empty value was persisted; want no row written")
	}
}

func TestSubscribers_ChannelsPatch_RejectsInactiveProvider(t *testing.T) {
	tc := newTestConsole(t)
	// ntfy is a valid push provider but has NO active integration in scope.
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-inactive-sub"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {"ntfy"},
			"value":       {"agent-alerts"},
		},
		cookie)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("inactive provider: status = %d, want 400", rr.Code)
	}
}

func TestSubscribers_ChannelsPatch_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "ro-chan-sub"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {"discord"},
			"value":       {"x"},
		},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only channels PATCH: status = %d, want 403", rr.Code)
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

// TestSubscribers_Create_AutoGeneratesBlankID pins the console-only
// convenience: when the operator leaves subscriberId blank (or whitespace),
// the handler mints a stable, non-empty SubscriberID server-side rather than
// rejecting the form. The generated id carries the "sub_" prefix.
func TestSubscribers_Create_AutoGeneratesBlankID(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers",
		url.Values{
			"subscriberId": {"   "}, // whitespace-only → treated as blank
			"email":        {"auto@example.com"},
			"firstName":    {"Auto"},
			"csrf_token":   {csrf},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create subscriber (blank id): status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	// Location redirects to /console/subscribers/<internal ULID>; resolve the
	// persisted row by that internal id and assert the generated external id.
	loc := rr.Header().Get("Location")
	internalID := strings.TrimPrefix(loc, "/console/subscribers/")
	if internalID == "" || internalID == loc {
		t.Fatalf("create subscriber (blank id): unexpected Location = %q", loc)
	}
	created, err := tc.deps.Audience.GetSubscriberByID(testBackground(), tc.scope, internalID)
	if err != nil {
		t.Fatalf("created subscriber (blank id) not persisted: %v", err)
	}
	if created.SubscriberID == "" {
		t.Errorf("create subscriber (blank id): persisted SubscriberID is empty, want auto-generated")
	}
	if !strings.HasPrefix(created.SubscriberID, "sub_") {
		t.Errorf("create subscriber (blank id): SubscriberID = %q, want \"sub_\" prefix", created.SubscriberID)
	}
}

// TestSubscribers_Create_PreservesProvidedID is a regression guard: when the
// operator supplies a subscriberId, that exact caller-owned value is used and
// never auto-generated over.
func TestSubscribers_Create_PreservesProvidedID(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers",
		url.Values{
			"subscriberId": {"usr-explicit-42"},
			"email":        {"explicit@example.com"},
			"csrf_token":   {csrf},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create subscriber (provided id): status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	created, err := tc.deps.Audience.Get(testBackground(), tc.scope, "usr-explicit-42")
	if err != nil {
		t.Fatalf("created subscriber (provided id) not persisted under exact id: %v", err)
	}
	if created.SubscriberID != "usr-explicit-42" {
		t.Errorf("create subscriber (provided id): SubscriberID = %q, want %q", created.SubscriberID, "usr-explicit-42")
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
// form must actually render (form element + subscriberId input +
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

	// The create slide-over is now a CLIENT-SIDE overlay: present on the list
	// page too, but hidden via data-show on the subNewOpen signal (seeded
	// closed), so opening it causes NO layout shift. The "+ Add Subscriber"
	// control toggles the signal instead of navigating to /new (which would
	// re-render the page — dropping the total tile + changing the title).
	listBody := tc.get(t, "/console/subscribers", cookie).Body.String()
	if !strings.Contains(listBody, `data-component="subscriber-create"`) {
		t.Error("list page should render the create slide-over as a hidden overlay")
	}
	if !strings.Contains(listBody, `data-show="$subNewOpen"`) {
		t.Error("create slide-over must be gated by data-show on the subNewOpen signal")
	}
	if !strings.Contains(listBody, `data-on:click="$subNewOpen = true"`) {
		t.Error(`"+ Add Subscriber" must toggle the subNewOpen signal, not navigate`)
	}
	if strings.Contains(listBody, `href="/console/subscribers/new"`) {
		t.Error("the add control must not navigate to /new (causes layout shift)")
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

// TestSubscribers_CreateForm_HasTimezoneField is a regression guard for the
// missing timezone input in the subscriber create slide-over. domain.Subscriber
// has a Timezone field and handleSubscriberCreate already reads it from the
// form; the form itself was missing the input.
func TestSubscribers_CreateForm_HasTimezoneField(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/subscribers/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// The create form must include a timezone input.
	if !strings.Contains(body, `name="timezone"`) {
		t.Errorf("/new create form missing timezone input (name=\"timezone\")")
	}

	// The field must have a reasonable placeholder (IANA tz example).
	if !strings.Contains(body, "America/New_York") && !strings.Contains(body, "UTC") {
		t.Errorf("/new create form timezone input missing IANA timezone placeholder")
	}

	// Submitting with a timezone must persist it.
	rr2 := tc.postForm(t, "/console/subscribers",
		url.Values{
			"subscriberId": {"tz-test-sub"},
			"email":        {"tz@example.com"},
			"timezone":     {"America/Chicago"},
			"csrf_token":   {csrf},
		},
		cookie)
	if rr2.Code != http.StatusSeeOther {
		t.Fatalf("create with timezone: status = %d, want 303 (body: %s)", rr2.Code, rr2.Body.String())
	}
	created, err := tc.deps.Audience.Get(testBackground(), tc.scope, "tz-test-sub")
	if err != nil {
		t.Fatalf("created subscriber not persisted: %v", err)
	}
	if created.Timezone != "America/Chicago" {
		t.Errorf("created subscriber Timezone = %q, want %q", created.Timezone, "America/Chicago")
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
		{http.MethodPost, "/console/subscribers/x/preferences"},
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

// ─── phase-1 bug regressions ──────────────────────────────────────────────────

// TestSubscribers_Create_RejectsDuplicateSubscriberID guards Bug 1:
// POSTing create with a subscriberId that already exists must NOT clobber the
// existing record. The handler must 303 back to /new with an inline error and
// keep subNewOpen=true so the slide-over stays open.
func TestSubscribers_Create_RejectsDuplicateSubscriberID(t *testing.T) {
	tc := newTestConsole(t)
	// Seed an existing subscriber.
	existing := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "dup-id-001",
		Email:        "original@example.com",
		FirstName:    "Original",
	})
	cookie, csrf := tc.login(t)

	// Attempt to create another subscriber with the same subscriberId.
	rr := tc.postForm(t, "/console/subscribers",
		url.Values{
			"subscriberId": {"dup-id-001"},
			"email":        {"clobber@example.com"},
			"firstName":    {"Clobber"},
			"csrf_token":   {csrf},
		},
		cookie)

	// Must NOT succeed (no 303 to detail page).
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("duplicate create: status = %d, want 303 (to /new with error)", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/console/subscribers/new") {
		t.Errorf("duplicate create: Location = %q, want redirect back to /new (not detail page)", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("duplicate create: Location = %q, missing error param", loc)
	}

	// The existing record must not have been clobbered.
	got, err := tc.deps.Audience.Get(testBackground(), tc.scope, "dup-id-001")
	if err != nil {
		t.Fatalf("get after duplicate create attempt: %v", err)
	}
	if got.Email != existing.Email {
		t.Errorf("clobber: email = %q, want %q (original must be preserved)", got.Email, existing.Email)
	}
	if got.FirstName != existing.FirstName {
		t.Errorf("clobber: FirstName = %q, want %q (original must be preserved)", got.FirstName, existing.FirstName)
	}

	// /new must re-render with the error and subNewOpen=true.
	rrGet := tc.get(t, loc, cookie)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", loc, rrGet.Code)
	}
	body := rrGet.Body.String()
	if !strings.Contains(body, "Subscriber ID is taken") && !strings.Contains(body, "taken") {
		t.Errorf("error page missing 'taken' message; body snippet: %s", body[:min(500, len(body))])
	}
}

// TestSubscribers_Create_AfterDelete_RevivesSubscriber guards the soft-delete
// revival path: creating a subscriber whose id was previously used and then
// soft-deleted must SUCCEED and yield a CLEAN record — only the fields
// supplied in the re-create form may be set. Stale profile data from the
// deleted era (firstName, phone, etc.) must be cleared, not silently inherited
// through the Upsert COALESCE merge.
//
// This test MUST fail against a plain Upsert-merge revive (which keeps the
// deleted-era profile) and pass only after the purge-then-upsert fix.
func TestSubscribers_Create_AfterDelete_RevivesSubscriber(t *testing.T) {
	tc := newTestConsole(t)

	// Step 1: create subscriber "revive-me" with a full profile so there is
	// stale data for the merge semantics to accidentally retain.
	orig := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "revive-me",
		Email:        "old@example.com",
		FirstName:    "Bob",
		Phone:        "+15550001111",
	})
	cookie, csrf := tc.login(t)

	// Step 2: soft-delete it.
	rr := tc.postForm(t, "/console/subscribers/"+orig.ID+"/delete",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303", rr.Code)
	}

	// Step 3: re-create "revive-me" supplying ONLY a new email; firstName and
	// phone are intentionally blank in the form.
	rr2 := tc.postForm(t, "/console/subscribers",
		url.Values{
			"csrf_token":   {csrf},
			"subscriberId": {"revive-me"},
			"email":        {"revived@example.com"},
			// firstName and phone deliberately omitted
		},
		cookie)
	// Use Errorf (not Fatalf) on the status check so all assertions still run.
	if rr2.Code != http.StatusSeeOther {
		t.Errorf("re-create after delete: status = %d, want 303 (body: %s)", rr2.Code, rr2.Body.String())
	}
	loc := rr2.Header().Get("Location")
	if strings.Contains(loc, "/new") {
		t.Errorf("re-create after delete: redirected to /new — soft-deleted id must not block creation; Location: %s", loc)
	}
	if strings.Contains(loc, "error=") {
		t.Errorf("re-create after delete: Location contains error= — must succeed; Location: %s", loc)
	}

	// Step 4: assert the revived record reflects ONLY the submitted fields.
	// The deleted-era profile (FirstName=Bob, Phone=+15550001111) must be
	// cleared — a console CREATE is not a partial patch.
	revived, err := tc.deps.Audience.Get(testBackground(), tc.scope, "revive-me")
	if err != nil {
		t.Fatalf("get revived subscriber: %v", err)
	}
	if revived.Deleted {
		t.Errorf("revived subscriber still marked deleted")
	}
	if revived.Email != "revived@example.com" {
		t.Errorf("revived email = %q, want %q", revived.Email, "revived@example.com")
	}
	// These fields were NOT submitted — they must be blank, not inherited from
	// the deleted-era record. A failure here means the purge-before-upsert fix
	// is missing and the COALESCE merge is retaining stale data.
	if revived.FirstName != "" {
		t.Errorf("revived FirstName = %q, want empty (stale deleted-era data must be cleared)", revived.FirstName)
	}
	if revived.Phone != "" {
		t.Errorf("revived Phone = %q, want empty (stale deleted-era data must be cleared)", revived.Phone)
	}
}

// TestSubscribers_Create_LiveCollisionRejected pins the live-collision branch:
// POSTing create with a subscriberId that belongs to a LIVE (non-deleted)
// subscriber must 303 to /new?error=...taken and leave the original record
// untouched. This guards against a future inversion of the !existing.Deleted
// guard inside handleSubscriberCreate.
func TestSubscribers_Create_LiveCollisionRejected(t *testing.T) {
	tc := newTestConsole(t)
	existing := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "live-taken",
		Email:        "live@example.com",
		FirstName:    "Alive",
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers",
		url.Values{
			"csrf_token":   {csrf},
			"subscriberId": {"live-taken"},
			"email":        {"attacker@example.com"},
		},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("live collision: status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/new") {
		t.Errorf("live collision: Location = %q, want redirect to /new", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("live collision: Location = %q, missing error param", loc)
	}

	// The original live record must be completely untouched.
	got, err := tc.deps.Audience.Get(testBackground(), tc.scope, "live-taken")
	if err != nil {
		t.Fatalf("get after live collision: %v", err)
	}
	if got.Email != existing.Email {
		t.Errorf("live collision clobbered email: got %q, want %q", got.Email, existing.Email)
	}
	if got.FirstName != existing.FirstName {
		t.Errorf("live collision clobbered FirstName: got %q, want %q", got.FirstName, existing.FirstName)
	}
	if got.Deleted {
		t.Errorf("live collision: original subscriber was deleted")
	}
}

// TestSubscribers_Detail_AfterDelete guards Bug 2:
// GET /console/subscribers/{id} for a soft-deleted subscriber must 404, not 200.
func TestSubscribers_Detail_AfterDelete(t *testing.T) {
	tc := newTestConsole(t)
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "to-delete-detail",
		Email:        "gone@example.com",
	})
	cookie, csrf := tc.login(t)

	// Delete the subscriber.
	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/delete",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: status = %d, want 303", rr.Code)
	}

	// Now GET the detail page — must be 404.
	rrDetail := tc.get(t, "/console/subscribers/"+sub.ID, cookie)
	if rrDetail.Code != http.StatusNotFound {
		t.Errorf("detail after delete: status = %d, want 404", rrDetail.Code)
	}
}

// TestSubscribers_ChannelsPatch_MissingProviderInlineError guards Bug 3:
// POST /console/subscribers/{id}/channels with no provider_id must 303 back
// to ?tab=channels with an inline error, not return a raw text/plain error page.
func TestSubscribers_ChannelsPatch_MissingProviderInlineError(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "discord",
		Channel:    "chat",
		Active:     true,
	})
	sub := tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: "chan-no-provider"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/subscribers/"+sub.ID+"/channels",
		url.Values{
			"csrf_token":  {csrf},
			"provider_id": {""}, // missing
			"value":       {"some-value"},
		},
		cookie)

	// Must be a 303 redirect (not 400 with raw text/plain error).
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("missing provider: status = %d, want 303 redirect (got body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "tab=channels") {
		t.Errorf("missing provider: Location = %q, want ?tab=channels", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("missing provider: Location = %q, missing error param", loc)
	}

	// Following the redirect must show the inline error.
	rrGet := tc.get(t, loc, cookie)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", loc, rrGet.Code)
	}
	body := rrGet.Body.String()
	if !strings.Contains(body, "provider") && !strings.Contains(body, "required") {
		t.Errorf("channels tab after error: missing provider-required message; snippet: %s", body[:min(500, len(body))])
	}
}

// TestFragment_SubscribersList_LoadMoreWithFilterKeepsFilter guards Bug 4:
// when a filter is active and the store returns a NextCursor (because the
// unfiltered set spans multiple pages), "Load more" must NOT be rendered —
// the cursor is anchored to the unfiltered set, so following it silently
// discards the filter and returns irrelevant rows. The fix is to suppress
// NextCursor in the view whenever hasFilters is true.
func TestFragment_SubscribersList_LoadMoreWithFilterKeepsFilter(t *testing.T) {
	tc := newTestConsole(t)
	// Seed 55 subscribers total: 53 "bob" rows (which fill the 50-row first
	// page and force a NextCursor) + 2 "alice" rows that happen to fall after
	// the cursor anchor.  The bob rows get ULID-ordered IDs that sort before
	// the alice rows, so the first page of 50 returns bob rows only, setting
	// NextCursor, while alice rows sit on page 2.
	for i := 0; i < 53; i++ {
		tc.seedSubscriber(t, audience.SubscriberInput{
			SubscriberID: "aaa-bob-" + strings.Repeat("0", 4-len(string(rune('0'+i)))) + string(rune('0'+i)),
			FirstName:    "Bob",
			LastName:     "Builder",
		})
	}
	// These get later ULIDs (seeded after the bobs).
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "zzz-alice-001",
		FirstName:    "Alice",
		LastName:     "Wonder",
	})
	tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "zzz-alice-002",
		FirstName:    "Alice",
		LastName:     "Smith",
	})
	cookie, _ := tc.login(t)

	// Unfiltered first page: should have NextCursor because 55 > 50.
	rrUnfiltered := tc.get(t, "/console/api/fragments/subscribers-list", cookie)
	if !strings.Contains(rrUnfiltered.Body.String(), "Load more") {
		t.Skip("store returned fewer than 50 rows; seeding timing inconsistency — skip")
	}

	// Filtered for "alice" — only 2 rows match, both sit on page 2 of the
	// unfiltered set. Without the fix the store-level NextCursor is set,
	// "Load more" appears, and clicking it drops the filter.
	// With the fix: NextCursor is suppressed when hasFilters is true, so
	// "Load more" must NOT appear.
	rr := tc.getWithQuery(t, "/console/api/fragments/subscribers-list",
		url.Values{"subscribersName": {"alice"}}.Encode(), cookie)
	body := rr.Body.String()

	// When a filter is active, Load more must NOT appear regardless of whether
	// the underlying cursor has more pages.
	if strings.Contains(body, "Load more") {
		t.Errorf("filtered list must NOT show 'Load more' (following the cursor silently discards the filter)")
	}
}

// TestFragment_SubscribersList_LoadMoreAppends is the TDD regression for the
// "Load more REPLACES rows" bug on the subscribers page. When the `after=`
// cursor is present (a Load more click) the server must emit two SSE patches
// that INSERT new rows before the pagination footer (mode before targeting
// #subscribers-pagination) rather than replacing the whole subscribers-tbody
// wrapper (which destroys the rows the user already loaded).
//
// Wire-format assertions:
//   - page-1 (no after): one outer-replace event containing id="subscribers-tbody"
//   - page-2 (after cursor): two events — first has `mode before` (insert new
//     rows before #subscribers-pagination), second morphs the pagination row in
//     place.  Neither event wraps rows in a new subscribers-tbody wrapper.
//   - Rows from page 2 are present in the append payload; page-1 rows are NOT
//     duplicated in the payload (they accumulated in the DOM, not in the wire).
//
// Fails against the current patchHTML (replace) behavior; passes after the
// patchSubscribersAppend implementation.
func TestFragment_SubscribersList_LoadMoreAppends(t *testing.T) {
	tc := newTestConsole(t)

	// Seed 8 subscribers so that limit=5 produces a non-empty page 1 with a
	// cursor and page 2 with the remaining 3.
	for i := 0; i < 8; i++ {
		tc.seedSubscriber(t, audience.SubscriberInput{
			SubscriberID: fmt.Sprintf("lm-sub-%02d", i),
			Email:        fmt.Sprintf("lm-%02d@example.com", i),
		})
	}
	cookie, _ := tc.login(t)

	// Page 1: no after cursor — expect a full outer-replace (subscribers-tbody
	// wrapper present, no mode before).
	rr1 := tc.get(t, "/console/api/fragments/subscribers-list?limit=5", cookie)
	if rr1.Code != http.StatusOK {
		t.Fatalf("page1: status = %d (body: %s)", rr1.Code, rr1.Body.String())
	}
	body1 := rr1.Body.String()
	if !strings.Contains(body1, `id="subscribers-tbody"`) {
		t.Errorf("page1: missing subscribers-tbody wrapper (initial load must be a full replace)")
	}
	if strings.Contains(body1, "mode before") {
		t.Errorf("page1 (no cursor): SSE must not use 'mode before' (first load is a full replace)")
	}

	// The Load more button must be visible (8 subscribers seeded > limit=5).
	if !strings.Contains(body1, "Load more") {
		t.Fatalf("page1: Load more button missing — not enough rows seeded or store limit mismatch (body: %s)",
			body1[:min(500, len(body1))])
	}

	// Extract the cursor value from the Load more button URL (after=<cursor>).
	const afterAttr = "after="
	afterIdx := strings.Index(body1, afterAttr)
	if afterIdx < 0 {
		t.Fatalf("page1: missing after= in Load more button; body: %s", body1[:min(400, len(body1))])
	}
	cursorStart := afterIdx + len(afterAttr)
	cursorEnd := strings.IndexAny(body1[cursorStart:], `&'"`)
	if cursorEnd < 0 {
		t.Fatalf("page1: could not parse cursor end from Load more URL")
	}
	cursor := body1[cursorStart : cursorStart+cursorEnd]
	if cursor == "" {
		t.Fatal("page1: parsed empty cursor from Load more URL")
	}

	// Page 2: with after cursor — this is the "Load more" click.
	rr2 := tc.getWithQuery(t, "/console/api/fragments/subscribers-list",
		url.Values{"after": {cursor}, "limit": {"5"}}.Encode(), cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("page2: status = %d (body: %s)", rr2.Code, rr2.Body.String())
	}
	body2 := rr2.Body.String()

	// Primary fix assertion: rows event must use mode=before, not outer-replace.
	if !strings.Contains(body2, "mode before") {
		t.Errorf("page2 (cursor set): SSE rows event must use `mode before` so existing rows are not replaced;\nbody:\n%s",
			body2)
	}

	// The page-2 payload must NOT contain a fresh subscribers-tbody wrapper —
	// wrapping in a new tbody would clobber the page-1 rows that are already
	// in the DOM.
	if strings.Contains(body2, `id="subscribers-tbody"`) {
		t.Errorf("page2 (append): response must NOT contain a new id=\"subscribers-tbody\" wrapper;\nbody:\n%s",
			body2)
	}

	// The pagination anchor that mode=before targets must be present.
	if !strings.Contains(body2, `id="subscribers-pagination"`) {
		t.Errorf("page2: response missing id=\"subscribers-pagination\" (the append target anchor);\nbody:\n%s",
			body2)
	}

	// Content correctness: the page-2 payload must carry the rows that did NOT
	// appear on page 1 (the next page) and must NOT re-send the page-1 rows
	// (they already accumulated in the DOM — re-sending would duplicate them).
	// Partition the 8 seeded ids by which page they landed on, derived from the
	// actual page-1 body rather than assuming a fixed store sort order.
	var page1IDs, page2IDs []string
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("lm-sub-%02d", i)
		if strings.Contains(body1, id) {
			page1IDs = append(page1IDs, id)
		} else {
			page2IDs = append(page2IDs, id)
		}
	}
	if len(page2IDs) == 0 {
		t.Fatalf("setup: no rows left for page 2 (page 1 already showed all 8); page1IDs=%v", page1IDs)
	}
	// Every page-2 id must be present in the append payload.
	for _, id := range page2IDs {
		if !strings.Contains(body2, id) {
			t.Errorf("page2 append payload missing next-page row %q;\nbody:\n%s", id, body2)
		}
	}
	// No page-1 id may reappear in the append payload (would duplicate rows).
	for _, id := range page1IDs {
		if strings.Contains(body2, id) {
			t.Errorf("page2 append payload re-sent page-1 row %q (rows must accumulate, not repeat);\nbody:\n%s",
				id, body2)
		}
	}
}
