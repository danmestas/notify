package console

// activity.go renders the /console/activity page (W1-6): the Workflow Runs
// activity feed with filter bar, cursor-paginated table fragment, and
// per-row detail panel fragment. All data is read in-process via
// Deps.Notifications (store.NotificationRepo) and Deps.Messages
// (store.MessageRepo). No SSE live-tail (moved to Wave 4 per gate 4.2);
// rows load on demand with "Load more" cursor pagination.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

const (
	activityDefaultLimit = 20
	activityMaxLimit     = 50
)

// ─── page-level view ────────────────────────────────────────────────────────

// activityView is what activity.html binds.
type activityView struct {
	Header   pageHeader
	Filters  activityFilters
	Signals  template.JS      // server-marshaled Datastar signals (see signalsJSON)
	Fragment activityListView // initial fragment inlined on first render
}

// signalsJSON marshals the filter values into the Datastar signals object
// Go-side. Building the JSON with encoding/json (rather than interpolating
// each value into a single-quoted data-signals attribute) means a filter
// value containing a double-quote cannot break out of the JSON and inject
// arbitrary signal keys. Returned as template.JS so html/template emits it
// verbatim into the attribute without HTML-entity-mangling the JSON.
func (f activityFilters) signalsJSON() template.JS {
	sig := map[string]string{
		"activityRange":          f.Range,
		"activityWorkflow":       f.WorkflowID,
		"activityChannel":        f.Channel,
		"activitySubscriber":     f.SubscriberID,
		"activityTransaction":    f.TransactionID,
		"activityStatus":         f.Status,
		"selectedNotificationId": "",
	}
	b, err := json.Marshal(sig)
	if err != nil { // map[string]string never fails to marshal
		return template.JS("{}")
	}
	return template.JS(b)
}

// activityFilters holds the current filter-bar values for echoing back into
// the form controls. All fields are strings; invalid values simply yield
// zero results rather than 400s.
type activityFilters struct {
	// Range is the time-range pill: "1h", "6h", "24h", "7d", "30d", or
	// "" meaning all time.
	Range         string
	WorkflowID    string
	Channel       string
	SubscriberID  string
	TransactionID string
	Status        string
}

// hasFilters reports whether any filter deviates from the default (all).
func (f activityFilters) hasFilters() bool {
	return f.Range != "" || f.WorkflowID != "" || f.Channel != "" ||
		f.SubscriberID != "" || f.TransactionID != "" || f.Status != ""
}

// ─── list fragment view ─────────────────────────────────────────────────────

// activityListView is the #activity-tbody fragment binding.
type activityListView struct {
	Rows       []activityRow
	Cursor     string // next-page token; empty = last page
	PrevCursor string // echoed from request so "Load more" carries it
	TotalCount int
	Limit      int
	From       int // 1-based offset of the first row shown (0 when paging)
	Filters    activityFilters
	HasFilters bool
}

// activityRow is one <tr> in the feed table.
type activityRow struct {
	// ID is the notification's opaque store id; set as data-id on the <tr>.
	ID                 string
	WorkflowID         string
	WorkflowName       string // human name from authoring; falls back to WorkflowID
	SubscriberID       string
	TransactionID      string
	TransactionIDShort string // first 26 chars for display
	Channel            string // derived from first message (best-effort)
	Status             string // derived from messages (best-effort)
	StatusTone         tileTone
	CreatedAt          string // RFC3339Nano raw
	RelativeTime       string // "3 minutes ago" etc.
	RunID              string
	RunURL             string // empty when DagnatsConsoleURL is unset
}

// ─── detail panel fragment view ─────────────────────────────────────────────

// activityDetailView is the #activity-detail-panel fragment binding.
type activityDetailView struct {
	Notification domain.Notification
	Messages     []domain.Message
	RunURL       string // empty when DagnatsConsoleURL is unset
	Empty        bool   // true when no notification is selected or found
}

// ─── signal envelope extensions ─────────────────────────────────────────────

// activitySignalKeys is the set of signal keys the activity feed reads from
// the Datastar envelope (one per filter pill). Passed to mergeFragmentParams.
var activitySignalKeys = []string{
	"activityRange", "activityWorkflow", "activityChannel",
	"activitySubscriber", "activityTransaction", "activityStatus",
	"selectedNotificationId",
}

// ─── helpers ────────────────────────────────────────────────────────────────

// activityFiltersFromQuery extracts filter values from a merged query map.
func activityFiltersFromQuery(q map[string][]string) activityFilters {
	get := func(key string) string {
		if vs := q[key]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	return activityFilters{
		Range:         get("range"),
		WorkflowID:    get("workflow_id"),
		Channel:       get("channel"),
		SubscriberID:  get("subscriber_id"),
		TransactionID: get("transaction_id"),
		Status:        get("status"),
	}
}

// notificationFilterFromActivity converts filter bar values to a
// store.NotificationFilter. Unknown range labels are logged and ignored
// by the caller.
func notificationFilterFromActivity(f activityFilters) (store.NotificationFilter, error) {
	nf := store.NotificationFilter{
		WorkflowID:    f.WorkflowID,
		SubscriberID:  f.SubscriberID,
		TransactionID: f.TransactionID,
	}
	if f.Range != "" {
		since, err := rangeToSince(f.Range)
		if err != nil {
			return nf, err
		}
		nf.Since = since
	}
	return nf, nil
}

// rangeToSince maps a time-range label to a since boundary.
func rangeToSince(r string) (time.Time, error) {
	now := time.Now().UTC()
	switch r {
	case "1h":
		return now.Add(-1 * time.Hour), nil
	case "6h":
		return now.Add(-6 * time.Hour), nil
	case "24h":
		return now.Add(-24 * time.Hour), nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), nil
	case "30d":
		return now.Add(-30 * 24 * time.Hour), nil
	}
	return time.Time{}, fmt.Errorf("unknown range %q", r)
}

// parseActivityLimit reads ?limit= from a merged query; defaults to
// activityDefaultLimit, capped at activityMaxLimit.
func parseActivityLimit(q map[string][]string) int {
	if vs := q["limit"]; len(vs) > 0 {
		if n, err := strconv.Atoi(vs[0]); err == nil && n > 0 {
			if n > activityMaxLimit {
				return activityMaxLimit
			}
			return n
		}
	}
	return activityDefaultLimit
}

// shortTxID truncates a transaction id to 26 chars for display.
func shortTxID(id string) string {
	if len(id) <= 26 {
		return id
	}
	return id[:26]
}

// relativeTime formats a RFC3339Nano timestamp as a human-friendly string.
func relativeTime(raw string) string {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	d := time.Since(t)
	switch {
	case d < 2*time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	}
	return t.UTC().Format("Jan 2, 2006")
}

// channelFromMessages returns the first non-empty channel seen in msgs.
func channelFromMessages(msgs []domain.Message) string {
	for _, m := range msgs {
		if m.Channel != "" {
			return m.Channel
		}
	}
	return ""
}

// statusFromMessages derives the composite status from a notification's
// messages: "failed" if any message failed, otherwise the newest message's
// status, or "pending" when there are no messages yet.
func statusFromMessages(msgs []domain.Message) string {
	if len(msgs) == 0 {
		return "pending"
	}
	for _, m := range msgs {
		if m.Status == "failed" || m.Status == "error" || m.Status == "bounced" {
			return "failed"
		}
	}
	return msgs[0].Status
}

// buildActivityRow converts a domain.Notification and its messages into a
// display row. workflowName and runURL are pre-resolved by the caller.
func (c *Console) buildActivityRow(n domain.Notification, msgs []domain.Message, workflowName, runURL string) activityRow {
	if c == nil {
		panic("buildActivityRow: c is nil")
	}
	channel := channelFromMessages(msgs)
	status := statusFromMessages(msgs)
	name := workflowName
	if name == "" {
		name = n.WorkflowID
	}
	return activityRow{
		ID:                 n.ID,
		WorkflowID:         n.WorkflowID,
		WorkflowName:       name,
		SubscriberID:       n.SubscriberID,
		TransactionID:      n.TransactionID,
		TransactionIDShort: shortTxID(n.TransactionID),
		Channel:            channel,
		Status:             status,
		StatusTone:         statusTone(status),
		CreatedAt:          n.CreatedAt,
		RelativeTime:       relativeTime(n.CreatedAt),
		RunID:              n.RunID,
		RunURL:             runURL,
	}
}

// fetchMessagesForNotification retrieves messages for a notification.
// GAP-AF-04 (MessageRepo filter-by-notification_id, issue #43) is not yet
// shipped, so this fans out one Messages.List per known channel and filters
// the results by RunID in-process — O(channels) queries per row, each
// returning all of that subscriber+channel's messages (query amplification;
// unbounded per subscriber). Acceptable only for Wave 1's small datasets;
// the indexed notification_id query in #43 replaces it. If RunID is empty,
// returns nil (no messages to show).
func (c *Console) fetchMessagesForNotification(ctx context.Context, scope domain.Scope, n domain.Notification) ([]domain.Message, error) {
	if c == nil {
		panic("fetchMessagesForNotification: c is nil")
	}
	if n.RunID == "" {
		return nil, nil
	}
	channels := []string{"in_app", "email", "push", "sms", "chat"}
	var matched []domain.Message
	for _, ch := range channels {
		msgs, err := c.deps.Messages.List(ctx, scope, n.SubscriberID, ch)
		if err != nil {
			return nil, fmt.Errorf("list messages channel %s: %w", ch, err)
		}
		for _, m := range msgs {
			if m.RunID == n.RunID {
				matched = append(matched, m)
			}
		}
	}
	return matched, nil
}

// workflowName resolves a human-readable name for workflowID via authoring.
// Returns workflowID on any error so the display always has something.
func (c *Console) workflowName(ctx context.Context, scope domain.Scope, workflowID string) string {
	if c == nil {
		panic("workflowName: c is nil")
	}
	if workflowID == "" {
		return ""
	}
	wf, err := c.deps.Authoring.GetBySlug(ctx, scope, workflowID)
	if err != nil {
		return workflowID
	}
	return wf.Name
}

// buildActivityListView fetches one page of notifications and builds rows.
func (c *Console) buildActivityListView(ctx context.Context, scope domain.Scope, filters activityFilters, cursor string, limit int) (activityListView, error) {
	if c == nil {
		panic("buildActivityListView: c is nil")
	}
	nf, err := notificationFilterFromActivity(filters)
	if err != nil {
		c.deps.Logger.Warn("console: activity filter range ignored",
			"range", filters.Range, "err", err)
		nf.Since = time.Time{}
	}

	page, err := c.deps.Notifications.List(ctx, scope, nf, cursor, limit)
	if err != nil {
		return activityListView{}, fmt.Errorf("list notifications: %w", err)
	}

	rows := make([]activityRow, 0, len(page.Items))
	for _, n := range page.Items {
		msgs, msgErr := c.fetchMessagesForNotification(ctx, scope, n)
		if msgErr != nil {
			c.deps.Logger.Warn("console: fetch messages for notification",
				"notification_id", n.ID, "err", msgErr)
			msgs = nil
		}
		wfName := c.workflowName(ctx, scope, n.WorkflowID)
		rows = append(rows, c.buildActivityRow(n, msgs, wfName, c.runURL(n.RunID)))
	}

	from := 1
	if cursor != "" {
		from = 0 // absolute offset unknown without a count-before-cursor query
	}

	return activityListView{
		Rows:       rows,
		Cursor:     page.Cursor,
		PrevCursor: cursor,
		TotalCount: page.TotalCount,
		Limit:      limit,
		From:       from,
		Filters:    filters,
		HasFilters: filters.hasFilters(),
	}, nil
}

// ─── page handler ────────────────────────────────────────────────────────────

// serveActivityPage renders GET /console/activity — the full page with
// filter bar and the first (unfiltered) list page inlined.
func (c *Console) serveActivityPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveActivityPage: w is nil")
	}
	if r == nil {
		panic("serveActivityPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveActivityPage: no scope in context (route not behind sessionMiddleware)")
	}

	q := mergeFragmentParams(r, activitySignalKeys...)
	filters := activityFiltersFromQuery(q)
	listView, err := c.buildActivityListView(r.Context(), scope, filters, "", parseActivityLimit(q))
	if err != nil {
		c.deps.Logger.Error("console: activity page build list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Activity",
		Subtitle: "Recent workflow runs for this environment.",
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: activity header", "err", err)
		header = pageHeader{Title: "Activity"}
	}
	c.renderPage(w, r, http.StatusOK, "activity", "Activity", activityView{
		Header:   header,
		Filters:  filters,
		Signals:  filters.signalsJSON(),
		Fragment: listView,
	})
}

// ─── list fragment handler ────────────────────────────────────────────────────

// serveFragmentActivityList answers GET /console/api/fragments/activity-list.
// Filter signals + cursor come from the merged query map; the response is one
// PatchElements SSE event repainting #activity-tbody.
func (c *Console) serveFragmentActivityList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentActivityList: w is nil")
	}
	if r == nil {
		panic("serveFragmentActivityList: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentActivityList: no scope in context")
	}

	q := mergeFragmentParams(r, activitySignalKeys...)
	filters := activityFiltersFromQuery(q)
	cursor := ""
	if vs := q["cursor"]; len(vs) > 0 {
		cursor = vs[0]
	}
	limit := parseActivityLimit(q)

	listView, err := c.buildActivityListView(r.Context(), scope, filters, cursor, limit)
	if err != nil {
		// A crafted ?cursor= is a client error, not a server fault — answer
		// 400 so it doesn't inflate error monitoring as a 500.
		if errors.Is(err, store.ErrInvalidCursor) {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		c.deps.Logger.Error("console: activity list fragment", "err", err)
		http.Error(w, "build activity list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "activity-tbody", listView)
	if err != nil {
		c.deps.Logger.Error("console: render activity list fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── detail panel fragment handler ──────────────────────────────────────────

// serveFragmentActivityDetail answers GET /console/api/fragments/activity-detail.
// ?notification_id= (or the $selectedNotificationId signal) selects the row.
// Returns #activity-detail-panel as a PatchElements SSE event.
func (c *Console) serveFragmentActivityDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentActivityDetail: w is nil")
	}
	if r == nil {
		panic("serveFragmentActivityDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentActivityDetail: no scope in context")
	}

	q := mergeFragmentParams(r, "selectedNotificationId")
	notifID := ""
	if vs := q["notification_id"]; len(vs) > 0 {
		notifID = vs[0]
	}
	// Accept the Datastar signal name as a fallback.
	if notifID == "" {
		if vs := q["selectedNotificationId"]; len(vs) > 0 {
			notifID = vs[0]
		}
	}

	renderEmpty := func() {
		html, err := renderFragment(c.ts.base, "activity-detail-panel",
			activityDetailView{Empty: true})
		if err != nil {
			c.deps.Logger.Error("console: render empty detail panel", "err", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		c.patchHTML(w, r, html)
	}

	if notifID == "" {
		renderEmpty()
		return
	}

	n, err := c.deps.Notifications.Get(r.Context(), scope, notifID)
	if err != nil {
		c.deps.Logger.Warn("console: activity detail get notification",
			"id", notifID, "err", err)
		renderEmpty()
		return
	}

	msgs, err := c.fetchMessagesForNotification(r.Context(), scope, n)
	if err != nil {
		c.deps.Logger.Warn("console: activity detail fetch messages",
			"notification_id", n.ID, "err", err)
		msgs = nil
	}

	html, err := renderFragment(c.ts.base, "activity-detail-panel", activityDetailView{
		Notification: n,
		Messages:     msgs,
		RunURL:       c.runURL(n.RunID),
	})
	if err != nil {
		c.deps.Logger.Error("console: render detail panel", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}
