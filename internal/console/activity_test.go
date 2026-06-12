// activity_test.go proves the W1-6 activity feed page and fragment
// endpoints against a seeded store. Pattern follows dashboard_test.go:
// production services over a temp SQLite store, full login/session stack,
// and white-box template assertions.
package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// ─── page render ─────────────────────────────────────────────────────────────

func TestActivity_PageRendersWithSeededNotifications(t *testing.T) {
	tc := newTestConsole(t)

	// Seed a notification so the table has a row.
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:    "welcome",
		SubscriberID:  "sub-001",
		TransactionID: "txn-abc123",
		RunID:         "run-xyz",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/activity: status = %d, want 200 (body: %s)",
			rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Page title present.
	if !strings.Contains(body, "Activity") {
		t.Errorf("activity page missing title")
	}
	// Nav entry is active.
	if !strings.Contains(body, `href="/console/activity"`) {
		t.Errorf("activity page missing nav link")
	}
	// Subscriber ID appears in the table.
	if !strings.Contains(body, n.SubscriberID) {
		t.Errorf("activity page missing subscriber id %q", n.SubscriberID)
	}
	// Transaction ID (truncated) appears.
	if !strings.Contains(body, shortTxID(n.TransactionID)) {
		t.Errorf("activity page missing transaction id %q", shortTxID(n.TransactionID))
	}
	// Filter bar rendered.
	if !strings.Contains(body, "activityRange") {
		t.Errorf("activity page missing filter bar signals")
	}
	// tbody fragment target id present.
	if !strings.Contains(body, `id="activity-tbody"`) {
		t.Errorf("activity page missing #activity-tbody")
	}
	// Detail panel target id present.
	if !strings.Contains(body, `id="activity-detail-panel"`) {
		t.Errorf("activity page missing #activity-detail-panel")
	}
}

func TestActivity_PageEmptyStoreShowsEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/activity: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No workflow runs yet") {
		t.Errorf("empty activity page missing empty-state copy")
	}
}

func TestActivity_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/activity", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /console/activity: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("redirect location = %q, want /console/login", got)
	}
}

// ─── list fragment ────────────────────────────────────────────────────────────

func TestFragment_ActivityList_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "welcome",
		SubscriberID: "sub-001",
		RunID:        "run-1",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("fragment missing SSE event marker")
	}
	if !strings.Contains(body, `id="activity-tbody"`) {
		t.Errorf("fragment missing stable patch target id")
	}
	if !strings.Contains(body, "sub-001") {
		t.Errorf("fragment missing seeded subscriber id")
	}
}

func TestFragment_ActivityList_FilterBySubscriberID(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-match",
		RunID:        "run-a",
	})
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-other",
		RunID:        "run-b",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-list?subscriber_id=sub-match", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("filtered fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-match") {
		t.Errorf("filtered fragment missing sub-match")
	}
	if strings.Contains(body, "sub-other") {
		t.Errorf("filtered fragment leaked sub-other")
	}
}

func TestFragment_ActivityList_FilterBySubscriberSignal(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-signal",
		RunID:        "run-sig",
	})
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-noise",
		RunID:        "run-noise",
	})
	cookie, _ := tc.login(t)

	// Signal rides the ?datastar= query param — same pattern as fragments_test.go.
	v := url.Values{}
	v.Set("datastar", `{"activitySubscriber":"sub-signal"}`)
	rr := tc.get(t, "/console/api/fragments/activity-list?"+v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("signal-filtered fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-signal") {
		t.Errorf("signal-filtered fragment missing sub-signal")
	}
	if strings.Contains(body, "sub-noise") {
		t.Errorf("signal-filtered fragment leaked sub-noise")
	}
}

func TestFragment_ActivityList_CursorWalk(t *testing.T) {
	tc := newTestConsole(t)
	// Seed 5 notifications with deterministic created_at ordering.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		tc.seedNotification(t, domain.Notification{
			WorkflowID:   "wf",
			SubscriberID: fmt.Sprintf("sub-%d", i),
			RunID:        fmt.Sprintf("run-%d", i),
			CreatedAt:    base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano),
		})
	}
	cookie, _ := tc.login(t)

	// Page 1: limit=3 → should return 3 rows + non-empty cursor.
	rr := tc.get(t, "/console/api/fragments/activity-list?limit=3", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("page1: status = %d", rr.Code)
	}
	body1 := rr.Body.String()
	if !strings.Contains(body1, `id="activity-tbody"`) {
		t.Errorf("page1 missing activity-tbody")
	}
	// "Load more" button must appear (there are 5 rows, page shows 3).
	if !strings.Contains(body1, "Load more") {
		t.Errorf("page1 missing Load more button (cursor pagination)")
	}
	// Extract the cursor from the Load more button href.
	const cursorAttr = "cursor="
	cursorIdx := strings.Index(body1, cursorAttr)
	if cursorIdx < 0 {
		t.Fatalf("page1 Load more button missing cursor= param")
	}
	cursorStart := cursorIdx + len(cursorAttr)
	cursorEnd := strings.IndexAny(body1[cursorStart:], `&'"`)
	if cursorEnd < 0 {
		t.Fatalf("page1 could not parse cursor end")
	}
	cursor := body1[cursorStart : cursorStart+cursorEnd]

	// Page 2: fetch with cursor → should return remaining 2 rows, no Load more.
	rr2 := tc.get(t, "/console/api/fragments/activity-list?limit=3&cursor="+cursor, cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("page2: status = %d", rr2.Code)
	}
	body2 := rr2.Body.String()
	if strings.Contains(body2, "Load more") {
		t.Errorf("page2 (last page) should not show Load more")
	}
}

func TestFragment_ActivityList_EmptyWithFiltersShowsFilterEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-list?subscriber_id=nobody", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("filter empty: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No runs match your filters") {
		t.Errorf("filter empty state missing copy; body snippet: %s",
			body[:min(200, len(body))])
	}
}

// ─── detail panel fragment ────────────────────────────────────────────────────

func TestFragment_ActivityDetail_EmptyWhenNoIDGiven(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail (no id): status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="activity-detail-panel"`) {
		t.Errorf("detail panel missing stable id")
	}
	if !strings.Contains(body, "Select a run") {
		t.Errorf("empty detail panel missing placeholder copy")
	}
}

func TestFragment_ActivityDetail_RendersNotification(t *testing.T) {
	tc := newTestConsole(t)
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:    "welcome",
		SubscriberID:  "sub-detail",
		TransactionID: "txn-detail-001",
		RunID:         "run-detail",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="activity-detail-panel"`) {
		t.Errorf("detail panel missing stable id")
	}
	if !strings.Contains(body, n.TransactionID) {
		t.Errorf("detail panel missing transaction id")
	}
	if !strings.Contains(body, n.SubscriberID) {
		t.Errorf("detail panel missing subscriber id")
	}
}

func TestFragment_ActivityDetail_DagnatsRunLinkRendered(t *testing.T) {
	tc := newTestConsole(t)
	// Wire a DagnatsConsoleURL so runURL returns a real link.
	tc.deps.DagnatsConsoleURL = "http://dagnats.local"
	// Re-build the console with the updated deps.
	tc.c = New(tc.deps)
	tc.h = tc.c.Routes()

	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-run",
		RunID:        "run-link-test",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail with run link: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "http://dagnats.local/console/runs/run-link-test") {
		t.Errorf("detail panel missing dagnats run link; body snippet: %s",
			body[:min(300, len(body))])
	}
}

func TestFragment_ActivityDetail_UnknownIDRendersEmpty(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id=does-not-exist", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown id detail: status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Select a run") {
		t.Errorf("unknown id detail panel missing empty placeholder")
	}
}

func TestFragment_ActivityDetail_ShowsSeededMessages(t *testing.T) {
	tc := newTestConsole(t)
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-msgs",
		RunID:        "run-msg-test",
	})
	// Seed a message for this notification's subscriber + run.
	tc.seedMessage(t, domain.Message{
		WorkflowID:     n.WorkflowID,
		NotificationID: n.ID,
		RunID:          n.RunID,
		SubscriberID:   n.SubscriberID,
		Channel:        "email",
		Status:         "sent",
		TransactionID:  n.TransactionID,
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail with messages: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "email") {
		t.Errorf("detail panel missing channel badge for seeded message")
	}
	if !strings.Contains(body, "sent") {
		t.Errorf("detail panel missing status for seeded message")
	}
}

// ─── helper ──────────────────────────────────────────────────────────────────

// min is a local helper; Go 1.21 has builtin min but the codebase targets
// whatever go.mod specifies. Inline it to stay compatible.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestActivity_SignalsJSONQuoteSafe proves a filter value containing a
// double-quote is JSON-escaped server-side and cannot break out of the
// data-signals attribute to inject arbitrary Datastar signals.
func TestActivity_SignalsJSONQuoteSafe(t *testing.T) {
	f := activityFilters{SubscriberID: `x", "selectedNotificationId": "pwned`}
	js := string(f.signalsJSON())
	// The hostile breakout sequence must not appear verbatim (unescaped).
	if strings.Contains(js, `", "selectedNotificationId": "pwned`) {
		t.Fatalf("signalsJSON did not escape the injection: %s", js)
	}
	// It must still be valid JSON that round-trips the literal value.
	var got map[string]string
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("signalsJSON produced invalid JSON: %v\n%s", err, js)
	}
	if got["activitySubscriber"] != f.SubscriberID {
		t.Fatalf("subscriber value mangled: got %q want %q", got["activitySubscriber"], f.SubscriberID)
	}
	if got["selectedNotificationId"] != "" {
		t.Fatalf("injection set selectedNotificationId=%q", got["selectedNotificationId"])
	}
}

// TestFragment_ActivityList_InvalidCursorReturns400 proves a crafted cursor
// is a client error (400), not a server fault (500).
func TestFragment_ActivityList_InvalidCursorReturns400(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-list?cursor=not-a-real-cursor", cookie)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("garbage cursor: got %d, want 400", rr.Code)
	}
}
