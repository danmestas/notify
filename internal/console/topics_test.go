// topics_test.go proves the W2-C4 topic list + detail with subscriber
// management console pages. Pattern follows integrations_test.go.
//
// Test index:
//
//	TestTopics_PageRenderWithSeededData         — page renders rows from store
//	TestTopics_PageEmptyState                   — empty store → empty-state copy
//	TestTopics_UnauthenticatedRedirectsToLogin  — auth gate on list page
//	TestTopics_NavEntryIsLive                   — nav link visible + active
//	TestFragment_TopicsList_SSEFraming          — fragment has SSE event marker
//	TestFragment_TopicsList_SeededRows          — fragment shows seeded topic
//	TestFragment_TopicsList_FilterByName        — name filter narrows results
//	TestFragment_TopicsList_FilterByKey         — key filter narrows results
//	TestTopics_DetailPage_Overview              — detail page overview tab
//	TestTopics_DetailPage_SubscribersTab        — members tab shows added member
//	TestTopics_DetailPage_NotFound              — unknown key → 404
//	TestTopics_Create_ReadOnlyBlocked           — POST create → 403 on read-only
//	TestTopics_Update_ReadOnlyBlocked           — POST update → 403 on read-only
//	TestTopics_Delete_ReadOnlyBlocked           — POST delete → 403 on read-only
//	TestTopics_AddSubscriber_ReadOnlyBlocked    — POST add-member → 403 on read-only
//	TestTopics_RemoveSubscriber_ReadOnlyBlocked — POST remove-member → 403 on read-only
//	TestTopics_Create_Redirect                  — successful create → redirect to list
//	TestTopics_AddRemoveSubscriber              — add member + verify + remove member
//	TestTopics_ScopeIsolation                   — alt-scope rows not visible
//	TestTopics_RouteAuth                        — all new routes behind session gate
package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/audience"
)

// ─── seed helpers ─────────────────────────────────────────────────────────────

// seedTopic upserts a topic in tc.scope via the audience service.
func (tc *testConsole) seedTopic(t *testing.T, key, name string) {
	t.Helper()
	_, err := tc.deps.Audience.CreateTopic(testBackground(), tc.scope, key, name)
	if err != nil {
		t.Fatalf("seed topic %q: %v", key, err)
	}
}

// ─── page render ──────────────────────────────────────────────────────────────

func TestTopics_PageRenderWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "weekly-digest", "Weekly Digest")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/topics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/topics: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Topics") {
		t.Errorf("topics page missing title")
	}
	if !strings.Contains(body, "weekly-digest") {
		t.Errorf("topics page missing seeded topic key")
	}
	if !strings.Contains(body, "Weekly Digest") {
		t.Errorf("topics page missing seeded topic name")
	}
}

func TestTopics_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/topics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/topics empty: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No topics") {
		t.Errorf("empty topics page missing empty-state copy (body snippet: %s)", body[:min(300, len(body))])
	}
}

func TestTopics_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/topics", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/topics: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

func TestTopics_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/topics", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/topics: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/topics"`) {
		t.Errorf("topics page missing nav link")
	}
	if !strings.Contains(body, `is-active`) {
		t.Errorf("topics page nav link not marked as active")
	}
}

// ─── list fragment ────────────────────────────────────────────────────────────

func TestFragment_TopicsList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "sse-topic", "SSE Topic")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/topics-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("topics-list fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("topics-list fragment missing SSE event marker")
	}
}

func TestFragment_TopicsList_SeededRows(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "visible-topic", "Visible Topic")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/topics-list", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "visible-topic") {
		t.Errorf("topics-list fragment missing seeded topic key")
	}
	if !strings.Contains(body, "Visible Topic") {
		t.Errorf("topics-list fragment missing seeded topic name")
	}
}

func TestFragment_TopicsList_FilterByName(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "topic-alpha", "Alpha Topic")
	tc.seedTopic(t, "topic-beta", "Beta Topic")
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/topics-list",
		url.Values{"topicsName": {"alpha"}}.Encode(), cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "Alpha Topic") {
		t.Errorf("name-filtered topics list missing Alpha Topic")
	}
	if strings.Contains(body, "Beta Topic") {
		t.Errorf("name-filtered topics list leaked Beta Topic")
	}
}

func TestFragment_TopicsList_FilterByKey(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "key-matching", "Key Match")
	tc.seedTopic(t, "other-one", "Other One")
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/topics-list",
		url.Values{"topicsKey": {"key-match"}}.Encode(), cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "key-matching") {
		t.Errorf("key-filtered topics list missing key-matching")
	}
	if strings.Contains(body, "other-one") {
		t.Errorf("key-filtered topics list leaked other-one")
	}
}

// ─── detail page ──────────────────────────────────────────────────────────────

func TestTopics_DetailPage_Overview(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "detail-topic", "Detail Topic")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/topics/detail-topic", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET topic detail: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "detail-topic") {
		t.Errorf("topic detail page missing key")
	}
	if !strings.Contains(body, "Detail Topic") {
		t.Errorf("topic detail page missing name")
	}
}

func TestTopics_DetailPage_SubscribersTab(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "members-topic", "Members Topic")
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "member-sub-001",
		Email:        "member@example.com",
		FirstName:    "Member",
	})
	// Add the subscriber to the topic.
	if err := tc.deps.Audience.AddTopicSubscriber(testBackground(), tc.scope, "members-topic", sub.SubscriberID); err != nil {
		t.Fatalf("add topic subscriber: %v", err)
	}
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/topics/members-topic?tab=subscribers", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET topic subscribers tab: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "member-sub-001") {
		t.Errorf("topic subscribers tab missing seeded member subscriber ID")
	}
	if !strings.Contains(body, "member@example.com") {
		t.Errorf("topic subscribers tab missing seeded member email")
	}
}

func TestTopics_DetailPage_NotFound(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/topics/nonexistent-topic-key", cookie)
	if rr.Code != http.StatusNotFound {
		t.Errorf("topic detail with bad key: status = %d, want 404", rr.Code)
	}
}

// ─── mutation handlers ────────────────────────────────────────────────────────

func TestTopics_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/topics",
		url.Values{"key": {"ro-topic"}, "name": {"RO Topic"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only create topic: status = %d, want 403", rr.Code)
	}
}

func TestTopics_Update_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedTopic(t, "ro-update-topic", "RO Update")
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/topics/ro-update-topic",
		url.Values{"name": {"New Name"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only update topic: status = %d, want 403", rr.Code)
	}
}

func TestTopics_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedTopic(t, "ro-del-topic", "RO Del")
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/topics/ro-del-topic/delete",
		url.Values{},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only delete topic: status = %d, want 403", rr.Code)
	}
}

func TestTopics_AddSubscriber_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedTopic(t, "ro-add-topic", "RO Add")
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/topics/ro-add-topic/subscribers",
		url.Values{"subscriberId": {"some-sub"}},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only add subscriber: status = %d, want 403", rr.Code)
	}
}

func TestTopics_RemoveSubscriber_ReadOnlyBlocked(t *testing.T) {
	tc := newReadOnlyTestConsole(t)
	tc.seedTopic(t, "ro-remove-topic", "RO Remove")
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/topics/ro-remove-topic/subscribers/some-sub/remove",
		url.Values{},
		cookie)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read-only remove subscriber: status = %d, want 403", rr.Code)
	}
}

func TestTopics_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/topics",
		url.Values{"key": {"new-topic-001"}, "name": {"New Topic"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("create topic: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	// Create redirects to the new topic's detail page.
	if got, want := rr.Header().Get("Location"), "/console/topics/new-topic-001"; got != want {
		t.Errorf("create topic: Location = %q, want %q", got, want)
	}
}

func TestTopics_AddRemoveSubscriber(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "mgmt-topic", "Mgmt Topic")
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "mgmt-sub-001",
		Email:        "mgmt@example.com",
	})
	cookie, csrf := tc.login(t)

	// Add subscriber.
	rr := tc.postForm(t, "/console/topics/mgmt-topic/subscribers",
		url.Values{"subscriberId": {sub.SubscriberID}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add subscriber to topic: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	// Verify member appears in subscribers tab.
	rr = tc.get(t, "/console/topics/mgmt-topic?tab=subscribers", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "mgmt-sub-001") {
		t.Errorf("topic subscribers tab missing added member 'mgmt-sub-001'")
	}

	// Remove subscriber.
	rr = tc.postForm(t, "/console/topics/mgmt-topic/subscribers/"+sub.SubscriberID+"/remove",
		url.Values{"csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("remove subscriber from topic: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	// Verify member is gone from subscribers tab.
	rr = tc.get(t, "/console/topics/mgmt-topic?tab=subscribers", cookie)
	body = rr.Body.String()
	if strings.Contains(body, "mgmt-sub-001") {
		t.Errorf("topic subscribers tab still shows removed member 'mgmt-sub-001'")
	}
}

// ─── create slide-over ────────────────────────────────────────────────────────

// TestTopics_NewRendersCreateForm pins the /new slide-over: the create form
// must actually render (form element + required key input + CSRF token),
// not just return 200 with the plain list.
func TestTopics_NewRendersCreateForm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	rr := tc.get(t, "/console/topics/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/topics/new: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, `data-component="topic-create"`) {
		t.Errorf("/new missing topic-create slide-over")
	}
	if !strings.Contains(body, `<form method="POST" action="/console/topics"`) {
		t.Errorf("/new missing create form posting to /console/topics")
	}
	if !strings.Contains(body, `name="key"`) {
		t.Errorf("/new create form missing key input")
	}
	if !strings.Contains(body, `name="name"`) {
		t.Errorf("/new create form missing name input")
	}
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("/new create form missing CSRF token")
	}

	// The plain list page must NOT render the slide-over.
	rr = tc.get(t, "/console/topics", cookie)
	if strings.Contains(rr.Body.String(), `data-component="topic-create"`) {
		t.Errorf("/console/topics rendered the create slide-over without /new")
	}
}

// ─── two-step inline confirm ──────────────────────────────────────────────────

// TestTopics_DetailDelete_TwoStepConfirm pins the chassis confirm idiom on
// the topic detail page.
func TestTopics_DetailDelete_TwoStepConfirm(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "confirm-topic", "Confirm Topic")
	cookie, csrf := tc.login(t)

	// Step 1: default state — confirm link, no delete POST form.
	rr := tc.get(t, "/console/topics/confirm-topic", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "confirmDelete=true") {
		t.Errorf("topic detail missing confirmDelete link")
	}
	if strings.Contains(body, `action="/console/topics/confirm-topic/delete"`) {
		t.Errorf("default topic detail rendered the delete POST form before confirm")
	}
	if strings.Contains(body, "onsubmit") {
		t.Errorf("topic detail uses native onsubmit confirm")
	}

	// Step 2: confirm state — Cancel + real POST form with CSRF.
	rr = tc.get(t, "/console/topics/confirm-topic?confirmDelete=true", cookie)
	body = rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirm state missing 'Confirm delete' button")
	}
	if !strings.Contains(body, "Cancel") {
		t.Errorf("confirm state missing Cancel")
	}
	if !strings.Contains(body, `action="/console/topics/confirm-topic/delete"`) {
		t.Errorf("confirm state missing delete POST form")
	}
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("confirm-delete form missing CSRF token")
	}
}

// TestTopics_MemberRemove_TwoStepConfirm pins the two-step confirm on member
// removal in the subscribers tab.
func TestTopics_MemberRemove_TwoStepConfirm(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "rm-confirm-topic", "RM Confirm")
	sub := tc.seedSubscriber(t, audience.SubscriberInput{
		SubscriberID: "rm-confirm-sub",
	})
	if err := tc.deps.Audience.AddTopicSubscriber(testBackground(), tc.scope, "rm-confirm-topic", sub.SubscriberID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie, csrf := tc.login(t)

	removeAction := `action="/console/topics/rm-confirm-topic/subscribers/` + sub.SubscriberID + `/remove"`

	// Step 1: default state — confirm link, no remove POST form.
	rr := tc.get(t, "/console/topics/rm-confirm-topic?tab=subscribers", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "confirmRemove="+sub.SubscriberID) {
		t.Errorf("members tab missing confirmRemove link")
	}
	if strings.Contains(body, removeAction) {
		t.Errorf("default members tab rendered the remove POST form before confirm")
	}

	// Step 2: confirm state — Cancel + real POST form with CSRF.
	rr = tc.get(t, "/console/topics/rm-confirm-topic?tab=subscribers&confirmRemove="+sub.SubscriberID, cookie)
	body = rr.Body.String()
	if !strings.Contains(body, "Confirm remove") {
		t.Errorf("confirm state missing 'Confirm remove' button")
	}
	if !strings.Contains(body, removeAction) {
		t.Errorf("confirm state missing remove POST form")
	}
	if !strings.Contains(body, `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("confirm-remove form missing CSRF token")
	}
}

// TestFragment_TopicsList_ConfirmDeleteState pins the fragment-level
// two-step confirm: ?confirmDelete=<key> renders that row in confirm state.
func TestFragment_TopicsList_ConfirmDeleteState(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "frag-confirm-topic", "Frag Confirm")
	cookie, _ := tc.login(t)

	// Plain fragment: no confirm form rendered.
	rr := tc.get(t, "/console/api/fragments/topics-list", cookie)
	if strings.Contains(rr.Body.String(), "Confirm delete") {
		t.Errorf("plain fragment rendered confirm state without confirmDelete param")
	}

	// Confirm state for the row.
	rr = tc.getWithQuery(t, "/console/api/fragments/topics-list",
		url.Values{"confirmDelete": {"frag-confirm-topic"}}.Encode(), cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirm fragment missing 'Confirm delete' button")
	}
	if !strings.Contains(body, `action="/console/topics/frag-confirm-topic/delete"`) {
		t.Errorf("confirm fragment missing delete POST form for row")
	}
}

// ─── add-member validation ────────────────────────────────────────────────────

// TestTopics_AddSubscriber_EmptyRejected pins the non-empty guard: a blank or
// whitespace subscriberId must be a 400, never a phantom membership row.
func TestTopics_AddSubscriber_EmptyRejected(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "empty-add-topic", "Empty Add")
	cookie, csrf := tc.login(t)

	for _, val := range []string{"", "   "} {
		rr := tc.postForm(t, "/console/topics/empty-add-topic/subscribers",
			url.Values{"subscriberId": {val}, "csrf_token": {csrf}},
			cookie)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("add member with subscriberId=%q: status = %d, want 400", val, rr.Code)
		}
	}

	// No phantom membership row.
	ids, err := tc.deps.Audience.ListTopicSubscriberIDs(testBackground(), tc.scope, "empty-add-topic")
	if err != nil {
		t.Fatalf("list member ids: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("phantom membership rows created: %v", ids)
	}
}

// TestTopics_AddSubscriber_UnknownSubscriberRejected pins the existence
// guard: memberships can never reference a subscriber that does not exist
// in scope.
func TestTopics_AddSubscriber_UnknownSubscriberRejected(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "ghost-add-topic", "Ghost Add")
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/topics/ghost-add-topic/subscribers",
		url.Values{"subscriberId": {"no-such-subscriber"}, "csrf_token": {csrf}},
		cookie)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("add unknown member: status = %d, want 422", rr.Code)
	}

	ids, err := tc.deps.Audience.ListTopicSubscriberIDs(testBackground(), tc.scope, "ghost-add-topic")
	if err != nil {
		t.Fatalf("list member ids: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("phantom membership rows created: %v", ids)
	}
}

// ─── member count consistency ─────────────────────────────────────────────────

// TestTopics_GetTopic_MemberCountMatchesList pins defect 5 at the service
// level: a single-topic read must carry the same member count as the list.
func TestTopics_GetTopic_MemberCountMatchesList(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedTopic(t, "count-topic", "Count Topic")
	for _, id := range []string{"count-sub-1", "count-sub-2"} {
		tc.seedSubscriber(t, audience.SubscriberInput{SubscriberID: id})
		if err := tc.deps.Audience.AddTopicSubscriber(testBackground(), tc.scope, "count-topic", id); err != nil {
			t.Fatalf("add member %s: %v", id, err)
		}
	}

	single, err := tc.deps.Audience.GetTopic(testBackground(), tc.scope, "count-topic")
	if err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	if single.MemberCount != 2 {
		t.Errorf("GetTopic memberCount = %d, want 2", single.MemberCount)
	}

	list, err := tc.deps.Audience.ListTopics(testBackground(), tc.scope)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 topic in list, got %d", len(list))
	}
	if list[0].MemberCount != single.MemberCount {
		t.Errorf("list memberCount %d != single-get memberCount %d",
			list[0].MemberCount, single.MemberCount)
	}

	// The detail page tab badge reflects the wired count.
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/topics/count-topic", cookie)
	if !strings.Contains(rr.Body.String(), `<span class="chip chip-default">2</span>`) {
		t.Errorf("topic detail tab badge missing member count 2")
	}
}

// ─── scope isolation ──────────────────────────────────────────────────────────

func TestTopics_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	altScope := tc.altScope(t)

	tc.seedTopic(t, "my-topic", "My Topic")
	// Seed directly into altScope.
	if _, err := tc.deps.Audience.CreateTopic(testBackground(), altScope, "their-topic", "Their Topic"); err != nil {
		t.Fatalf("seed alt topic: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/topics", cookie)
	body := rr.Body.String()

	if !strings.Contains(body, "my-topic") {
		t.Errorf("topics page missing own topic 'my-topic'")
	}
	if strings.Contains(body, "their-topic") {
		t.Errorf("topics page leaked alt-scope topic 'their-topic'")
	}
}

// ─── route auth ───────────────────────────────────────────────────────────────

// TestTopics_RouteAuth verifies all topic routes redirect to login when called
// without a session cookie.
func TestTopics_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/console/topics"},
		{http.MethodGet, "/console/topics/new"},
		{http.MethodGet, "/console/topics/x"},
		{http.MethodGet, "/console/api/fragments/topics-list"},
		{http.MethodGet, "/console/api/fragments/subscriber-autocomplete"},
		{http.MethodPost, "/console/topics"},
		{http.MethodPost, "/console/topics/x"},
		{http.MethodPost, "/console/topics/x/delete"},
		{http.MethodPost, "/console/topics/x/subscribers"},
		{http.MethodPost, "/console/topics/x/subscribers/y/remove"},
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
