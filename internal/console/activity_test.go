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
	"github.com/danmestas/notify/internal/timestamp"
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
			CreatedAt:    timestamp.Format(base.Add(time.Duration(i) * time.Second)),
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

// ─── regression: channel and status filters were silently dropped ─────────────
//
// Before the fix, activityFiltersFromQuery correctly parsed channel/status from
// the merged query but notificationFilterFromActivity never forwarded them to
// the store layer (store.NotificationFilter has no Channel/Status fields —
// those are message-derived). The fix applies in-process post-filtering inside
// buildActivityListView after messages are fetched and channel/status are known.

// TestFragment_ActivityList_FilterByChannel seeds two notifications each with a
// message on a different channel and asserts the channel= query param filters rows.
func TestFragment_ActivityList_FilterByChannel(t *testing.T) {
	tc := newTestConsole(t)

	nEmail := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-email",
		RunID:        "run-email",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nEmail.WorkflowID,
		NotificationID: nEmail.ID,
		RunID:          nEmail.RunID,
		SubscriberID:   nEmail.SubscriberID,
		Channel:        "email",
		Status:         "sent",
	})

	nSMS := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-sms",
		RunID:        "run-sms",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nSMS.WorkflowID,
		NotificationID: nSMS.ID,
		RunID:          nSMS.RunID,
		SubscriberID:   nSMS.SubscriberID,
		Channel:        "sms",
		Status:         "sent",
	})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-list?channel=email", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("channel filter: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-email") {
		t.Errorf("channel=email filter missing sub-email row")
	}
	if strings.Contains(body, "sub-sms") {
		t.Errorf("channel=email filter leaked sub-sms row")
	}
}

// ─── regression: "All channels" silently excluded stream messages ─────────────
//
// Shape A added the "stream" channel (the "stream" step type's built-in feed
// publish) but fetchMessagesForNotification's hardcoded channel fan-out list
// and the filter-bar dropdown were never updated. "All channels" (no filter)
// undercounted vs Dashboard/Analytics, which correctly include stream.

// TestFragment_ActivityList_AllChannelsIncludesStream seeds an in_app message
// and a stream message on two different notifications and asserts both appear
// with no channel filter applied ("All channels" must include stream).
func TestFragment_ActivityList_AllChannelsIncludesStream(t *testing.T) {
	tc := newTestConsole(t)

	nInApp := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-allch-inapp",
		RunID:        "run-allch-inapp",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nInApp.WorkflowID,
		NotificationID: nInApp.ID,
		RunID:          nInApp.RunID,
		SubscriberID:   nInApp.SubscriberID,
		Channel:        "in_app",
		Status:         "sent",
	})

	nStream := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-allch-stream",
		RunID:        "run-allch-stream",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nStream.WorkflowID,
		NotificationID: nStream.ID,
		RunID:          nStream.RunID,
		SubscriberID:   nStream.SubscriberID,
		Channel:        "stream",
		Status:         "sent",
	})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("activity-list (all channels): status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-allch-inapp") {
		t.Errorf("all-channels view missing in_app row")
	}
	if !strings.Contains(body, "sub-allch-stream") {
		t.Errorf("all-channels view missing stream row entirely")
	}
	// The stream row's channel badge must show "stream", not the "unknown"
	// placeholder — before the fix, fetchMessagesForNotification's hardcoded
	// channel fan-out list omitted "stream", so the stream message was never
	// fetched and the row rendered with an empty/unknown channel.
	if !strings.Contains(body, `activity-channel-badge--stream`) {
		t.Errorf("all-channels view: stream row missing its stream channel badge — stream message never fetched, body: %.800s", body)
	}
}

// TestFragment_ActivityList_FilterByStreamChannel proves a "stream" channel
// filter value scopes the list to stream-channel rows only.
func TestFragment_ActivityList_FilterByStreamChannel(t *testing.T) {
	tc := newTestConsole(t)

	nInApp := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-strfilt-inapp",
		RunID:        "run-strfilt-inapp",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nInApp.WorkflowID,
		NotificationID: nInApp.ID,
		RunID:          nInApp.RunID,
		SubscriberID:   nInApp.SubscriberID,
		Channel:        "in_app",
		Status:         "sent",
	})

	nStream := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-strfilt-stream",
		RunID:        "run-strfilt-stream",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nStream.WorkflowID,
		NotificationID: nStream.ID,
		RunID:          nStream.RunID,
		SubscriberID:   nStream.SubscriberID,
		Channel:        "stream",
		Status:         "sent",
	})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-list?channel=stream", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("channel=stream filter: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-strfilt-stream") {
		t.Errorf("channel=stream filter missing sub-strfilt-stream row")
	}
	if strings.Contains(body, "sub-strfilt-inapp") {
		t.Errorf("channel=stream filter leaked sub-strfilt-inapp row")
	}
}

// TestActivity_ChannelDropdownHasStreamOption proves the channel filter
// dropdown offers a "Stream" option.
func TestActivity_ChannelDropdownHasStreamOption(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/activity", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/activity: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-value="stream"`) {
		t.Errorf("channel filter dropdown missing a stream option (data-value=\"stream\")")
	}
	if !strings.Contains(body, "Stream") {
		t.Errorf("channel filter dropdown missing \"Stream\" label")
	}
}

// TestFragment_ActivityList_FilterByChannelSignal proves the same filter
// via the Datastar activityChannel signal (the path the UI actually takes).
func TestFragment_ActivityList_FilterByChannelSignal(t *testing.T) {
	tc := newTestConsole(t)

	nEmail := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-sig-email",
		RunID:        "run-sig-email",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nEmail.WorkflowID,
		NotificationID: nEmail.ID,
		RunID:          nEmail.RunID,
		SubscriberID:   nEmail.SubscriberID,
		Channel:        "email",
		Status:         "sent",
	})

	nSMS := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-sig-sms",
		RunID:        "run-sig-sms",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nSMS.WorkflowID,
		NotificationID: nSMS.ID,
		RunID:          nSMS.RunID,
		SubscriberID:   nSMS.SubscriberID,
		Channel:        "sms",
		Status:         "sent",
	})

	cookie, _ := tc.login(t)
	v := url.Values{}
	v.Set("datastar", `{"activityChannel":"email"}`)
	rr := tc.get(t, "/console/api/fragments/activity-list?"+v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("activityChannel signal filter: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-sig-email") {
		t.Errorf("activityChannel=email signal filter missing sub-sig-email row")
	}
	if strings.Contains(body, "sub-sig-sms") {
		t.Errorf("activityChannel=email signal filter leaked sub-sig-sms row")
	}
}

// TestFragment_ActivityList_FilterByStatus seeds two notifications with
// messages of different statuses and asserts the status= query param filters rows.
func TestFragment_ActivityList_FilterByStatus(t *testing.T) {
	tc := newTestConsole(t)

	nSent := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-sent",
		RunID:        "run-sent",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nSent.WorkflowID,
		NotificationID: nSent.ID,
		RunID:          nSent.RunID,
		SubscriberID:   nSent.SubscriberID,
		Channel:        "email",
		Status:         "sent",
	})

	nFailed := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-failed",
		RunID:        "run-failed",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nFailed.WorkflowID,
		NotificationID: nFailed.ID,
		RunID:          nFailed.RunID,
		SubscriberID:   nFailed.SubscriberID,
		Channel:        "email",
		Status:         "failed",
	})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-list?status=sent", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status filter: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-sent") {
		t.Errorf("status=sent filter missing sub-sent row")
	}
	if strings.Contains(body, "sub-failed") {
		t.Errorf("status=sent filter leaked sub-failed row")
	}
}

// TestFragment_ActivityList_FilterByStatusSignal proves status filtering via the
// Datastar activityStatus signal (the path the UI actually takes).
func TestFragment_ActivityList_FilterByStatusSignal(t *testing.T) {
	tc := newTestConsole(t)

	nSent := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-sig-sent",
		RunID:        "run-sig-sent",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nSent.WorkflowID,
		NotificationID: nSent.ID,
		RunID:          nSent.RunID,
		SubscriberID:   nSent.SubscriberID,
		Channel:        "email",
		Status:         "sent",
	})

	nFailed := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-sig-failed",
		RunID:        "run-sig-failed",
	})
	tc.seedMessage(t, domain.Message{
		WorkflowID:     nFailed.WorkflowID,
		NotificationID: nFailed.ID,
		RunID:          nFailed.RunID,
		SubscriberID:   nFailed.SubscriberID,
		Channel:        "email",
		Status:         "failed",
	})

	cookie, _ := tc.login(t)
	v := url.Values{}
	v.Set("datastar", `{"activityStatus":"sent"}`)
	rr := tc.get(t, "/console/api/fragments/activity-list?"+v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("activityStatus signal filter: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-sig-sent") {
		t.Errorf("activityStatus=sent signal filter missing sub-sig-sent row")
	}
	if strings.Contains(body, "sub-sig-failed") {
		t.Errorf("activityStatus=sent signal filter leaked sub-sig-failed row")
	}
}

// TestFragment_ActivityList_LoadMoreAppends is the TDD regression for the
// "Load more REPLACES rows" bug. When cursor is set (a Load more click) the
// server must emit SSE patches that insert new rows before the pagination
// footer (mode before targeting #activity-pagination) rather than replacing
// the whole <tbody> (which destroys the rows the user already loaded).
//
// Wire-format assertions:
//   - page-1 (no cursor): one outer-replace event containing id="activity-tbody"
//   - page-2 (cursor):    two events — first has `mode before` (insert new rows
//     before #activity-pagination), second morphs the pagination row in place.
//     Neither event wraps rows in a new <tbody id="activity-tbody">.
func TestFragment_ActivityList_LoadMoreAppends(t *testing.T) {
	tc := newTestConsole(t)
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// Seed 4 notifications so page 1 (limit=2) has a cursor and page 2 exists.
	for i := 0; i < 4; i++ {
		tc.seedNotification(t, domain.Notification{
			WorkflowID:   "wf",
			SubscriberID: fmt.Sprintf("append-sub-%d", i),
			RunID:        fmt.Sprintf("append-run-%d", i),
			CreatedAt:    timestamp.Format(base.Add(time.Duration(i) * time.Second)),
		})
	}
	cookie, _ := tc.login(t)

	// Page 1: no cursor — expect a full tbody outer-replace (no mode= line in SSE).
	rr1 := tc.get(t, "/console/api/fragments/activity-list?limit=2", cookie)
	if rr1.Code != http.StatusOK {
		t.Fatalf("page1: status = %d (body: %s)", rr1.Code, rr1.Body.String())
	}
	body1 := rr1.Body.String()
	if !strings.Contains(body1, `id="activity-tbody"`) {
		t.Errorf("page1: missing activity-tbody wrapper (initial load must be a full replace)")
	}
	// Page 1 must NOT carry `mode before` — it is a full outer replace.
	if strings.Contains(body1, "mode before") {
		t.Errorf("page1 (no cursor): SSE must not use 'mode before' (first load is a full replace)")
	}

	// Extract cursor from page 1 Load more button.
	const cursorAttr = "cursor="
	cursorIdx := strings.Index(body1, cursorAttr)
	if cursorIdx < 0 {
		t.Fatalf("page1 missing cursor= in Load more button; body: %s", body1[:min(400, len(body1))])
	}
	cursorStart := cursorIdx + len(cursorAttr)
	cursorEnd := strings.IndexAny(body1[cursorStart:], `&'"`)
	if cursorEnd < 0 {
		t.Fatalf("page1 could not parse cursor end")
	}
	cursor := body1[cursorStart : cursorStart+cursorEnd]

	// Page 2: with cursor — this is the "Load more" click.
	// The SSE response MUST carry `mode before` (rows event inserts before
	// #activity-pagination) so existing rows stay in the DOM.
	rr2 := tc.get(t, "/console/api/fragments/activity-list?limit=2&cursor="+cursor, cookie)
	if rr2.Code != http.StatusOK {
		t.Fatalf("page2: status = %d (body: %s)", rr2.Code, rr2.Body.String())
	}
	body2 := rr2.Body.String()

	// Primary fix assertion: rows event must use mode=before, not outer-replace.
	if !strings.Contains(body2, "mode before") {
		t.Errorf("page2 (cursor set): SSE rows event must use `mode before` so existing rows are not replaced;\nbody:\n%s",
			body2[:min(600, len(body2))])
	}
	// The page-2 subscriber rows must be present in the response payload.
	// Rows are ordered created_at DESC, so page 1 returns append-sub-3 and
	// append-sub-2; page 2 returns append-sub-1 and append-sub-0. Assert on
	// a specific page-2 subscriber id so the check cannot pass by accidentally
	// matching a page-1 row that leaked into the response.
	if !strings.Contains(body2, "append-sub-1") {
		t.Errorf("page2 response missing page-2 subscriber row append-sub-1; body: %s", body2[:min(400, len(body2))])
	}
	// Page 2 must NOT wrap rows in a new <tbody id="activity-tbody"> — that
	// would replace the existing tbody content, defeating the append.
	if strings.Contains(body2, `id="activity-tbody"`) {
		t.Errorf("page2 append response must not contain a new <tbody id=activity-tbody>; "+
			"a nested tbody is invalid HTML and causes the existing rows to vanish.\nbody:\n%s",
			body2[:min(600, len(body2))])
	}
	// The pagination row must still be present (cursor advances or disappears).
	if !strings.Contains(body2, `id="activity-pagination"`) {
		t.Errorf("page2 response missing pagination row id=activity-pagination")
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

// ─── regression: Bug 1 — payload rendered as JSON text ───────────────────────

// TestFragment_ActivityDetail_PayloadRendersAsJSONText is the regression for
// the byte-slice payload rendering bug. Before the fix, Notification.Payload
// (json.RawMessage / []byte) was passed directly to html/template which
// formats []byte as its decimal byte-slice representation ("[123 34 …]")
// instead of the raw JSON text. The fix exposes PayloadText string in
// activityDetailView. This test seeds a notification with a JSON payload and
// asserts the rendered panel contains the JSON text, not a decimal byte list.
func TestFragment_ActivityDetail_PayloadRendersAsJSONText(t *testing.T) {
	tc := newTestConsole(t)
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-payload",
		RunID:        "run-payload",
		Payload:      json.RawMessage(`{"hello":"world"}`),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("detail: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The payload must appear as JSON text, not as a decimal byte-slice
	// ("[123 34 …]"). html/template HTML-escapes double-quotes as &#34; in
	// text context, so the rendered form of {"hello":"world"} is
	// {&#34;hello&#34;:&#34;world&#34;}.
	if !strings.Contains(body, "{&#34;hello&#34;:&#34;world&#34;}") {
		t.Errorf("detail panel missing JSON payload (expected HTML-escaped JSON text); body snippet:\n%s",
			body[:min(600, len(body))])
	}
	// The byte-slice rendering must be absent.
	if strings.Contains(body, "[123 34") {
		t.Errorf("detail panel contains raw byte-slice encoding of payload; body snippet:\n%s",
			body[:min(600, len(body))])
	}
}

// ─── regression: Bug 2 — selectedNotificationId absent from seeded signals ───

// TestActivity_SignalsJSONOmitsSelectedNotificationId is the regression for the
// detail-panel auto-close bug. Before the fix, signalsJSON() included
// "selectedNotificationId":"" in the data-signals attribute. Any re-processing
// of that attribute by the Datastar runtime (e.g. triggered by a list-fragment
// patch) would reset the signal to "" and close an open detail panel. The fix
// omits selectedNotificationId from signalsJSON() so that the signal is created
// lazily on the first row-click and is never reset by a list refresh.
func TestActivity_SignalsJSONOmitsSelectedNotificationId(t *testing.T) {
	f := activityFilters{Range: "1h", Status: "sent"}
	js := string(f.signalsJSON())

	var got map[string]string
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("signalsJSON produced invalid JSON: %v\n%s", err, js)
	}
	if _, present := got["selectedNotificationId"]; present {
		t.Errorf("signalsJSON must not seed selectedNotificationId (got %q); "+
			"seeding it causes Datastar to reset it to \"\" on list refreshes, closing the detail panel",
			got["selectedNotificationId"])
	}
	// Filter signals must still be present.
	if got["activityRange"] != "1h" {
		t.Errorf("activityRange missing or wrong: %q", got["activityRange"])
	}
	if got["activityStatus"] != "sent" {
		t.Errorf("activityStatus missing or wrong: %q", got["activityStatus"])
	}
}

// ─── regression: L1 — activity feed showed the raw workflow ULID ────────────
//
// Before the fix, workflowName looked up domain.Notification.WorkflowID (the
// workflow's internal ULID id — see trigger.go's Notification{WorkflowID:
// wf.ID}) via Authoring.GetBySlug, which expects the trigger identifier
// (slug), not the id. The lookup therefore always missed and silently fell
// back to the raw id, so both the list row and the run-detail panel rendered
// a bare ULID with no link. The fix resolves the id via Authoring.GetByID and
// links to the workflow's editor page.

// TestFragment_ActivityList_ShowsWorkflowNameAndLink seeds a real workflow via
// Authoring, seeds a notification pointing at its internal id (mirroring how
// trigger.Ingest actually populates Notification.WorkflowID), and asserts the
// list row renders the workflow's NAME (not its raw id) linked to its editor
// page.
func TestFragment_ActivityList_ShowsWorkflowNameAndLink(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "Welcome Series", "welcome-series")
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   wf.ID,
		SubscriberID: "sub-l1",
		RunID:        "run-l1",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Welcome Series") {
		t.Errorf("activity list missing resolved workflow name; body snippet:\n%s", body[:min(800, len(body))])
	}
	// The raw id is still allowed in a title="" tooltip attribute (same
	// convention as the other columns); the DISPLAYED link text must be the
	// name, not the id.
	if strings.Contains(body, ">"+wf.ID+"<") {
		t.Errorf("activity list displays the raw workflow ULID %q instead of the resolved name", wf.ID)
	}
	if !strings.Contains(body, `href="/console/workflows/welcome-series"`) {
		t.Errorf("activity list workflow name missing link to the editor page; body snippet:\n%s", body[:min(800, len(body))])
	}
}

// TestFragment_ActivityDetail_ShowsWorkflowNameAndLink is the run-detail-panel
// counterpart: the "Workflow" field must show the resolved name linked to the
// editor page, not the bare id.
func TestFragment_ActivityDetail_ShowsWorkflowNameAndLink(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "Payment Failed", "payment-failed")
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:   wf.ID,
		SubscriberID: "sub-l1-detail",
		RunID:        "run-l1-detail",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Payment Failed") {
		t.Errorf("detail panel missing resolved workflow name; body snippet:\n%s", body[:min(800, len(body))])
	}
	if !strings.Contains(body, `href="/console/workflows/payment-failed"`) {
		t.Errorf("detail panel workflow name missing link to the editor page; body snippet:\n%s", body[:min(800, len(body))])
	}
}

// TestFragment_ActivityDetail_UnknownWorkflowFallsBackToID proves a
// notification whose workflow was deleted (or never existed) degrades
// gracefully: the raw id is shown, with no link (never a broken href).
func TestFragment_ActivityDetail_UnknownWorkflowFallsBackToID(t *testing.T) {
	tc := newTestConsole(t)
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf-does-not-exist",
		SubscriberID: "sub-l1-orphan",
		RunID:        "run-l1-orphan",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "wf-does-not-exist") {
		t.Errorf("detail panel should fall back to the raw id for an unresolvable workflow; body snippet:\n%s", body[:min(800, len(body))])
	}
}

// TestFragment_ActivityDetail_PersistsAcrossListRefresh verifies that fetching
// the activity-list fragment (a list refresh) does NOT produce HTML that would
// reset the selectedNotificationId signal. Specifically, the list fragment
// response must not contain a data-signals attribute that declares
// selectedNotificationId. If it did, Datastar could reset the signal to ""
// and close an open detail panel.
func TestFragment_ActivityDetail_PersistsAcrossListRefresh(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedNotification(t, domain.Notification{
		WorkflowID:   "wf",
		SubscriberID: "sub-refresh",
		RunID:        "run-refresh",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/activity-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("list fragment: status = %d", rr.Code)
	}
	body := rr.Body.String()

	// The list fragment must NOT include a data-signals attribute that declares
	// selectedNotificationId as a JSON key (e.g. `"selectedNotificationId":""`).
	// Such a declaration would allow Datastar to reset the signal to "" and
	// close an open detail panel. Appearing in a data-on:click expression
	// (to SET the signal: $selectedNotificationId = '...') is fine and expected.
	// We match the JSON-key form `"selectedNotificationId":` to distinguish a
	// signal declaration from the click-handler expression.
	if strings.Contains(body, `"selectedNotificationId":`) {
		t.Errorf("list fragment contains a data-signals declaration of selectedNotificationId "+
			"(JSON key form); this can reset the signal and close an open detail panel.\nbody snippet:\n%s",
			body[:min(600, len(body))])
	}
}
