package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// workflow_versions.go covers the workflow lifecycle actions and the
// activity tab (W2-A6):
//
//   - POST /console/api/workflows/{slug}/delete           delete
//   - POST /console/api/workflows/{slug}/activate         active toggle
//   - POST /console/api/workflows/{slug}/rollback          restore a prior version
//   - GET  /console/workflows/{slug}/activity              activity tab page
//   - GET  /console/api/fragments/workflow-activity        activity tbody fragment
//
// Every mutation handler is gated on Deps.ReadOnly server-side and
// answers a native form POST with a 303 redirect (no SSE).

// workflowActivityTabView is what the workflow activity tab page binds (W2-A6).
type workflowActivityTabView struct {
	Header            pageHeader
	TriggerIdentifier string
	// WorkflowID is the internal workflow id (not the slug). The SSE
	// live-tail (W4-W2) filters on it because the notification store keys
	// runs by internal id, so the delivered message's WorkflowID is the
	// internal id too. Passed to the stream as ?workflow=<id>.
	WorkflowID string
	Name       string
	Active     bool
	Fragment   wfActivityListView
	Signals    template.JS
	ReadOnly   bool
}

// wfActivityListView is the workflow-activity-tbody fragment binding.
type wfActivityListView struct {
	Slug         string // workflow trigger identifier (for filter links)
	Rows         []activityRow
	Cursor       string
	PrevCursor   string
	TotalCount   int
	Limit        int
	HasFilters   bool
	FilterStatus string
	FilterRange  string
}

// wfActivitySignalKeys are the signal keys the workflow activity fragment reads.
var wfActivitySignalKeys = []string{"wfActStatus", "wfActRange"}

// handleWorkflowDelete handles POST /console/api/workflows/{slug}/delete.
func (c *Console) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowDelete: w is nil")
	}
	if r == nil {
		panic("handleWorkflowDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	if err := c.deps.Authoring.Delete(r.Context(), scope, slug); err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete workflow", "slug", slug, "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/workflows", http.StatusSeeOther)
}

// handleWorkflowActivate handles POST /console/api/workflows/{slug}/activate.
// The "active" form field is "1" for activate, anything else for deactivate.
func (c *Console) handleWorkflowActivate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowActivate: w is nil")
	}
	if r == nil {
		panic("handleWorkflowActivate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowActivate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	active := r.FormValue("active") == "1"
	if _, err := c.deps.Authoring.SetActive(r.Context(), scope, slug, active); err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: set workflow active", "slug", slug, "err", err)
		http.Error(w, "activate failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/workflows/"+slug, http.StatusSeeOther)
}

// handleWorkflowRollback handles POST /console/api/workflows/{slug}/rollback:
// re-registers an earlier published version's compiled DAG byte-for-byte via
// authoring.Service.Rollback, which itself scope-checks the version id
// against the workflow resolved from slug (a version id valid in this scope
// but belonging to a different workflow 404s rather than cross-wiring one
// workflow's DAG onto another's trigger).
func (c *Console) handleWorkflowRollback(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowRollback: w is nil")
	}
	if r == nil {
		panic("handleWorkflowRollback: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowRollback: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	editURL := "/console/workflows/" + slug
	versionID := strings.TrimSpace(r.FormValue("versionId"))
	if versionID == "" {
		http.Redirect(w, r, editURL+"?error="+
			url.QueryEscape("Version is required."), http.StatusSeeOther)
		return
	}

	if err := c.deps.Authoring.Rollback(r.Context(), scope, slug, versionID); err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: rollback workflow", "slug", slug, "err", err)
		http.Redirect(w, r, editURL+"?error="+
			url.QueryEscape("Rollback failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, editURL, http.StatusSeeOther)
}

// ─── W2-A6: workflow activity tab ─────────────────────────────────────────────

// wfActivitySignalsJSON builds the initial Datastar signals for the workflow
// activity tab.
func wfActivitySignalsJSON(status, rangeVal string) template.JS {
	b, _ := json.Marshal(map[string]string{
		"wfActStatus": status,
		"wfActRange":  rangeVal,
	})
	return template.JS(b)
}

// buildWFActivityListView fetches one page of notifications for the given
// workflow slug and builds the activity rows.
func (c *Console) buildWFActivityListView(ctx context.Context, scope domain.Scope, slug, cursor string, limit int, filterStatus, filterRange string) (wfActivityListView, error) {
	if c == nil {
		panic("buildWFActivityListView: c is nil")
	}
	// Resolve the workflow to get its internal ID for the filter.
	wf, err := c.deps.Authoring.GetBySlug(ctx, scope, slug)
	if err != nil {
		return wfActivityListView{}, fmt.Errorf("get workflow: %w", err)
	}

	nf := store.NotificationFilter{WorkflowID: wf.ID}
	if filterRange != "" {
		since, err := rangeToSince(filterRange)
		if err != nil {
			c.deps.Logger.Warn("console: wf activity filter range ignored",
				"range", filterRange, "err", err)
		} else {
			nf.Since = since
		}
	}

	page, err := c.deps.Notifications.List(ctx, scope, nf, cursor, limit)
	if err != nil {
		return wfActivityListView{}, fmt.Errorf("list notifications: %w", err)
	}

	rows := make([]activityRow, 0, len(page.Items))
	for _, n := range page.Items {
		msgs, msgErr := c.fetchMessagesForNotification(ctx, scope, n)
		if msgErr != nil {
			c.deps.Logger.Warn("console: wf activity fetch messages",
				"notification_id", n.ID, "err", msgErr)
			msgs = nil
		}
		// This view is scoped to a single workflow (workflow_activity_row.html
		// has no Workflow column), so the editor link is not needed here.
		row := c.buildActivityRow(n, msgs, wf.Name, "", c.runURL(n.RunID))
		// Status filter is applied in-memory (the store filter does not yet
		// carry a status predicate — messages derive status, not notifications).
		if filterStatus != "" && row.Status != filterStatus {
			continue
		}
		rows = append(rows, row)
	}

	hasFilters := filterStatus != "" || filterRange != ""
	return wfActivityListView{
		Slug:         slug,
		Rows:         rows,
		Cursor:       page.Cursor,
		PrevCursor:   cursor,
		TotalCount:   page.TotalCount,
		Limit:        limit,
		HasFilters:   hasFilters,
		FilterStatus: filterStatus,
		FilterRange:  filterRange,
	}, nil
}

// serveWorkflowActivityPage renders GET /console/workflows/{slug}/activity (W2-A6).
func (c *Console) serveWorkflowActivityPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowActivityPage: w is nil")
	}
	if r == nil {
		panic("serveWorkflowActivityPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowActivityPage: no scope in context")
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		c.serveNotFound(w, r)
		return
	}
	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: wf activity page get workflow", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	q := mergeFragmentParams(r, wfActivitySignalKeys...)
	filterStatus := ""
	if vs := q["status"]; len(vs) > 0 {
		filterStatus = vs[0]
	}
	filterRange := ""
	if vs := q["range"]; len(vs) > 0 {
		filterRange = vs[0]
	}

	frag, err := c.buildWFActivityListView(r.Context(), scope, slug, "", activityDefaultLimit, filterStatus, filterRange)
	if err != nil {
		c.deps.Logger.Error("console: wf activity page build list", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    wf.Name,
		Subtitle: fmt.Sprintf("Trigger identifier: %s", wf.TriggerIdentifier),
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: wf.Name}
	}

	view := workflowActivityTabView{
		Header:            header,
		TriggerIdentifier: wf.TriggerIdentifier,
		WorkflowID:        wf.ID,
		Name:              wf.Name,
		Active:            wf.Active,
		Fragment:          frag,
		Signals:           wfActivitySignalsJSON(filterStatus, filterRange),
		ReadOnly:          c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "workflow-activity", wf.Name+" — Activity", view)
}

// serveFragmentWorkflowActivity answers GET /console/api/fragments/workflow-activity/{slug} (W2-A6).
func (c *Console) serveFragmentWorkflowActivity(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentWorkflowActivity: w is nil")
	}
	if r == nil {
		panic("serveFragmentWorkflowActivity: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentWorkflowActivity: no scope in context")
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}

	q := mergeFragmentParams(r, wfActivitySignalKeys...)
	filterStatus := ""
	if vs := q["status"]; len(vs) > 0 {
		filterStatus = vs[0]
	}
	filterRange := ""
	if vs := q["range"]; len(vs) > 0 {
		filterRange = vs[0]
	}
	cursor := ""
	if vs := q["cursor"]; len(vs) > 0 {
		cursor = vs[0]
	}
	limit := parseActivityLimit(q)

	frag, err := c.buildWFActivityListView(r.Context(), scope, slug, cursor, limit, filterStatus, filterRange)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		c.deps.Logger.Error("console: wf activity fragment", "slug", slug, "err", err)
		http.Error(w, "build list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "workflow-activity-tbody", frag)
	if err != nil {
		c.deps.Logger.Error("console: render wf activity fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}
