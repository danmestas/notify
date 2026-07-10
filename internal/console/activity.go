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
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
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
	// selectedNotificationId is intentionally absent here. Seeding it to ""
	// in data-signals causes Datastar to reset it to "" any time the
	// attribute is re-processed (e.g. during a list-fragment patch that
	// triggers re-evaluation of ancestor signal declarations), which would
	// close an open detail panel. The signal is created lazily on the first
	// row-click ($selectedNotificationId = 'id') and survives list refreshes
	// because it is no longer declared in the seeded signal map.
	sig := map[string]string{
		"activityRange":       f.Range,
		"activityWorkflow":    f.WorkflowID,
		"activityChannel":     f.Channel,
		"activitySubscriber":  f.SubscriberID,
		"activityTransaction": f.TransactionID,
		"activityStatus":      f.Status,
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
	ID           string
	WorkflowID   string
	WorkflowName string // human name from authoring; falls back to WorkflowID
	// WorkflowURL links to the workflow's editor page; empty when the
	// workflow no longer exists (deleted) so the template renders plain text.
	WorkflowURL        string
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
	// WorkflowName is the resolved human-readable name for
	// Notification.WorkflowID; falls back to the raw id when the workflow no
	// longer exists.
	WorkflowName string
	// WorkflowURL links to the workflow's editor page; empty when the
	// workflow no longer exists (deleted) so the template renders plain text.
	WorkflowURL string
	// PayloadText is the notification payload rendered as a UTF-8 string so
	// that html/template emits it verbatim. Notification.Payload is a
	// json.RawMessage ([]byte); html/template renders []byte as its decimal
	// byte-slice representation ([123 34 …]) rather than as JSON text.
	// The handler populates this field with string(Notification.Payload).
	PayloadText string
	Messages    []domain.Message
	RunURL      string // empty when DagnatsConsoleURL is unset
	Empty       bool   // true when no notification is selected or found
	// CSRF is the session token for the inline Resend form (W4 #55). Empty
	// when no session (renderEmpty path) — the form is only shown for a
	// selected notification.
	CSRF string
	// ReadOnly hides the Resend action when the console is read-only (W4 #55).
	ReadOnly bool
	// CanResend reports whether the durable resend action is wired
	// (Deps.Trigger != nil). When false the Resend button is omitted.
	CanResend bool
	// ConfirmResend renders the two-step inline confirm state for Resend
	// (chassis idiom: no native confirm() since it sends a real message).
	ConfirmResend bool
	// ResendRunURL / ResendError carry the post-resend banner: the deep-link
	// to the new engine run, or the failure message. Mutually exclusive.
	ResendRunURL string
	ResendError  string
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
// display row. workflowName, workflowURL, and runURL are pre-resolved by the
// caller.
func (c *Console) buildActivityRow(n domain.Notification, msgs []domain.Message, workflowName, workflowURL, runURL string) activityRow {
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
		WorkflowURL:        workflowURL,
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

// workflowInfo resolves a human-readable name and editor-page URL for
// workflowID (domain.Notification.WorkflowID: the workflow's internal ULID
// id — see trigger.go's Notification{WorkflowID: wf.ID} — NOT its trigger
// identifier/slug, so this must look up by id). Returns (workflowID, "") on
// any error — deleted workflow, empty id — so the display always has
// something and never links to a page that doesn't exist (L1).
func (c *Console) workflowInfo(ctx context.Context, scope domain.Scope, workflowID string) (name, url string) {
	if c == nil {
		panic("workflowInfo: c is nil")
	}
	if workflowID == "" {
		return "", ""
	}
	wf, err := c.deps.Authoring.GetByID(ctx, scope, workflowID)
	if err != nil {
		return workflowID, ""
	}
	name = wf.Name
	if name == "" {
		name = workflowID
	}
	return name, "/console/workflows/" + wf.TriggerIdentifier
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
		wfName, wfURL := c.workflowInfo(ctx, scope, n.WorkflowID)
		rows = append(rows, c.buildActivityRow(n, msgs, wfName, wfURL, c.runURL(n.RunID)))
	}

	// Channel and Status are derived from messages, not stored on notifications,
	// so they cannot be pushed into the store.NotificationFilter DB query.
	// Apply them in-process after row construction. W1: acceptable for small datasets.
	totalCount := page.TotalCount
	if filters.Channel != "" || filters.Status != "" {
		kept := rows[:0]
		for _, row := range rows {
			if filters.Channel != "" && row.Channel != filters.Channel {
				continue
			}
			if filters.Status != "" && row.Status != filters.Status {
				continue
			}
			kept = append(kept, row)
		}
		rows = kept
		// page.TotalCount is the UNFILTERED DB count; after in-process channel/
		// status filtering it would overstate the visible rows, so report the
		// filtered count instead.
		totalCount = len(rows)
	}

	from := 1
	if cursor != "" {
		from = 0 // absolute offset unknown without a count-before-cursor query
	}

	return activityListView{
		Rows:       rows,
		Cursor:     page.Cursor,
		PrevCursor: cursor,
		TotalCount: totalCount,
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

	// When a cursor is present the request is a "Load more" click: append new
	// rows before the existing pagination footer rather than replacing the
	// whole tbody (which would destroy the rows the user already loaded).
	// Two SSE events are emitted:
	//   1. new <tr> rows   — mode=before targeting #activity-pagination
	//   2. pagination <tr> — mode=outer (morph) to advance the cursor
	// When cursor is empty (initial load or filter change) a single outer
	// replace of the full #activity-tbody is the correct behavior.
	if cursor != "" {
		c.patchActivityAppend(w, r, listView)
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

// patchActivityAppend sends two Datastar SSE events for a "Load more" response:
//  1. The new data rows (activity-rows template) inserted before
//     #activity-pagination via mode=before — the rows land above the footer
//     while existing rows stay in place.
//  2. The updated pagination footer (activity-pagination template) morphed in
//     place via mode=outer so the cursor advances to the next page.
func (c *Console) patchActivityAppend(w http.ResponseWriter, r *http.Request, lv activityListView) {
	if w == nil {
		panic("patchActivityAppend: w is nil")
	}
	if r == nil {
		panic("patchActivityAppend: r is nil")
	}

	rowsHTML, err := renderFragment(c.ts.base, "activity-rows", lv)
	if err != nil {
		c.deps.Logger.Error("console: render activity-rows fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	paginHTML, err := renderFragment(c.ts.base, "activity-pagination", lv)
	if err != nil {
		c.deps.Logger.Error("console: render activity-pagination fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	sse := datastar.NewSSE(w, r)
	// Event 1: insert new rows before the pagination row so they accumulate above it.
	if err := sse.PatchElements(
		rowsHTML,
		datastar.WithSelector("#activity-pagination"),
		datastar.WithMode(datastar.ElementPatchModeBefore),
	); err != nil {
		c.deps.Logger.Error("console: patch activity rows (append)", "err", err)
		return
	}
	// Event 2: morph the pagination row in place to advance the cursor.
	if err := sse.PatchElements(paginHTML); err != nil {
		c.deps.Logger.Error("console: patch activity pagination", "err", err)
	}
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

	s, _ := sessionFromContext(r.Context())
	// resendRunURL is the deep-link to the new run minted by a just-completed
	// resend (the handler 303-redirects here with ?resendRun=<run_id>).
	resendRunURL := ""
	if rid := r.URL.Query().Get("resendRun"); rid != "" {
		resendRunURL = c.runURL(rid)
	}
	wfName, wfURL := c.workflowInfo(r.Context(), scope, n.WorkflowID)
	html, err := renderFragment(c.ts.base, "activity-detail-panel", activityDetailView{
		Notification:  n,
		WorkflowName:  wfName,
		WorkflowURL:   wfURL,
		PayloadText:   string(n.Payload),
		Messages:      msgs,
		RunURL:        c.runURL(n.RunID),
		CSRF:          s.CSRF,
		ReadOnly:      c.deps.ReadOnly,
		CanResend:     c.deps.Trigger != nil,
		ConfirmResend: r.URL.Query().Get("confirmResend") == "true",
		ResendRunURL:  resendRunURL,
		ResendError:   r.URL.Query().Get("resendError"),
	})
	if err != nil {
		c.deps.Logger.Error("console: render detail panel", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// handleNotificationResend handles POST /console/activity/{id}/resend
// (W4 ADR-013, #55): the durable resend-notification action. It re-fires the
// notification's workflow for the same subscriber and payload through the
// engine, yielding a new run, then 303-redirects back to the activity page with
// the new run surfaced via ?resendRun= (or ?resendError= on failure). This is a
// native form POST → 303 (never SSE); the inline two-step confirm guards the
// real send.
func (c *Console) handleNotificationResend(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleNotificationResend: w is nil")
	}
	if r == nil {
		panic("handleNotificationResend: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleNotificationResend: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	notifID := chi.URLParam(r, "id")
	if notifID == "" {
		http.Error(w, "missing notification id", http.StatusBadRequest)
		return
	}
	// The detail panel re-opens this notification after the redirect.
	backURL := "/console/activity?notification_id=" + url.QueryEscape(notifID)

	if c.deps.Trigger == nil {
		http.Redirect(w, r, backURL+"&resendError="+
			url.QueryEscape("Resend service unavailable."), http.StatusSeeOther)
		return
	}

	n, err := c.deps.Notifications.Get(r.Context(), scope, notifID)
	if err != nil {
		c.deps.Logger.Warn("console: resend get notification", "id", notifID, "err", err)
		http.Redirect(w, r, backURL+"&resendError="+
			url.QueryEscape("Notification not found."), http.StatusSeeOther)
		return
	}

	// The notification carries the internal workflow id; trigger.Ingest needs
	// the workflow's trigger identifier (slug) to re-fire it.
	wf, err := c.deps.Authoring.GetByID(r.Context(), scope, n.WorkflowID)
	if err != nil {
		c.deps.Logger.Warn("console: resend workflow lookup",
			"workflow_id", n.WorkflowID, "err", err)
		http.Redirect(w, r, backURL+"&resendError="+
			url.QueryEscape("Workflow for this notification no longer exists."),
			http.StatusSeeOther)
		return
	}

	payload := n.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	resp, err := c.deps.Trigger.Ingest(r.Context(), scope, trigger.TriggerRequest{
		Name:    wf.TriggerIdentifier,
		To:      trigger.ToField{SubscriberID: n.SubscriberID},
		Payload: payload,
	})
	if err != nil {
		c.deps.Logger.Error("console: resend ingest", "id", notifID, "err", err)
		http.Redirect(w, r, backURL+"&resendError="+
			url.QueryEscape("Resend failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	if !resp.Acknowledged {
		msg := "Resend not acknowledged."
		if len(resp.Error) > 0 {
			msg = resp.Error[0]
		}
		http.Redirect(w, r, backURL+"&resendError="+url.QueryEscape(msg),
			http.StatusSeeOther)
		return
	}

	// Resolve the new run id from the freshly-minted transaction so the panel
	// can deep-link it. The run id is written on the new notification row.
	newRunID := c.runIDFromTransaction(r.Context(), scope, resp.TransactionID)
	dest := backURL
	if newRunID != "" {
		dest += "&resendRun=" + url.QueryEscape(newRunID)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// runIDFromTransaction looks up the most recent notification for a transaction
// id and returns its run id (empty when not found). Mirrors
// runURLFromTransaction but returns the raw id so the caller can pass it through
// a redirect.
func (c *Console) runIDFromTransaction(ctx context.Context, scope domain.Scope, txid string) string {
	if c == nil || txid == "" {
		return ""
	}
	page, err := c.deps.Notifications.List(ctx, scope, store.NotificationFilter{TransactionID: txid}, "", 1)
	if err != nil || len(page.Items) == 0 {
		return ""
	}
	return page.Items[0].RunID
}
