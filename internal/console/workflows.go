package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// workflows.go renders:
//
//   - GET /console/workflows — full workflows list page
//   - GET /console/api/fragments/workflows-list — Datastar PatchElements
//     fragment (search + status filter, re-rendered tbody)
//   - GET /console/workflows/{slug} — read-only workflow detail page

// workflowRow is one row in the workflows-list table.
type workflowRow struct {
	// Name is the human display name.
	Name string
	// TriggerIdentifier is the slug shown in monospace + copy button.
	TriggerIdentifier string
	// Active is the source of the status chip.
	Active bool
	// Steps are the decoded domain steps for the step-type chips.
	Steps []domain.Step
	// Href is the link to the detail page.
	Href string
}

// workflowsListView is what workflows.html binds.
type workflowsListView struct {
	Header       pageHeader
	Rows         []workflowRow
	FilterQ      string
	FilterStatus string
	// HasFilters is true when any filter is active; the Reset link shows.
	HasFilters bool
	// Empty is the empty-state binding (nil when there are rows).
	Empty    *emptyState
	ReadOnly bool
}

// workflowDetailStepView is one step entry in the detail page.
type workflowDetailStepView struct {
	// StepID is the engine step identifier.
	StepID string
	// Type is the step channel/type string (in_app, email, delay, …).
	Type string
	// Name is the optional human label.
	Name string
	// ControlValuesJSON is the pretty-printed controlValues for display.
	ControlValuesJSON string
}

// workflowDetailView is what workflow_detail.html binds.
type workflowDetailView struct {
	Header            pageHeader
	ID                string
	Name              string
	TriggerIdentifier string
	Active            bool
	Steps             []workflowDetailStepView
	// PayloadSchemaJSON is the pretty-printed payload schema (empty when absent).
	PayloadSchemaJSON string
	ReadOnly          bool
}

// buildWorkflowRows decodes the steps JSON and filters rows matching q and status.
// Filtering is in-memory: workflow lists are scoped to one environment and small.
func buildWorkflowRows(workflows []domain.Workflow, q, status string) []workflowRow {
	q = strings.TrimSpace(strings.ToLower(q))
	rows := make([]workflowRow, 0, len(workflows))
	for _, wf := range workflows {
		if q != "" {
			if !strings.Contains(strings.ToLower(wf.Name), q) &&
				!strings.Contains(strings.ToLower(wf.TriggerIdentifier), q) {
				continue
			}
		}
		switch status {
		case "active":
			if !wf.Active {
				continue
			}
		case "inactive":
			if wf.Active {
				continue
			}
		}
		var steps []domain.Step
		if len(wf.Steps) > 0 {
			_ = json.Unmarshal(wf.Steps, &steps)
		}
		rows = append(rows, workflowRow{
			Name:              wf.Name,
			TriggerIdentifier: wf.TriggerIdentifier,
			Active:            wf.Active,
			Steps:             steps,
			Href:              "/console/workflows/" + wf.TriggerIdentifier,
		})
	}
	return rows
}

// serveWorkflowsList renders GET /console/workflows.
func (c *Console) serveWorkflowsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowsList: w is nil")
	}
	if r == nil {
		panic("serveWorkflowsList: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowsList: no scope in context (route not behind sessionMiddleware)")
	}
	workflows, err := c.deps.Authoring.List(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: list workflows", "err", err)
		c.serveError(w, r)
		return
	}

	// Full page always shows unfiltered count in the header tile.
	header := pageHeader{
		Title:    "Workflows",
		Subtitle: "All notification workflows in this environment.",
		Tiles: []tile{
			{Label: "workflows", Count: len(workflows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: workflows header", "err", err)
		header = pageHeader{Title: "Workflows"}
	}

	rows := buildWorkflowRows(workflows, "", "")
	view := workflowsListView{
		Header:   header,
		Rows:     rows,
		ReadOnly: c.deps.ReadOnly,
	}
	if len(rows) == 0 {
		view.Empty = workflowsEmptyState(false, c.deps.ReadOnly)
	}
	c.renderPage(w, r, http.StatusOK, "workflows", "Workflows", view)
}

// serveFragmentWorkflowsList answers GET /console/api/fragments/workflows-list.
// It reads wfFilter and wfStatus signals (or explicit URL params), filters
// the in-process workflow list, and returns a PatchElements SSE event
// replacing #workflows-tbody.
func (c *Console) serveFragmentWorkflowsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentWorkflowsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentWorkflowsList: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentWorkflowsList: no scope in context (route not behind sessionMiddleware)")
	}
	q := mergeFragmentParams(r, "wfFilter", "wfStatus")
	filterQ := ""
	if vs := q["q"]; len(vs) > 0 {
		filterQ = vs[0]
	}
	filterStatus := ""
	if vs := q["status"]; len(vs) > 0 {
		filterStatus = vs[0]
	}

	workflows, err := c.deps.Authoring.List(r.Context(), scope)
	if err != nil {
		c.deps.Logger.Error("console: fragment list workflows", "err", err)
		http.Error(w, "list workflows failed", http.StatusInternalServerError)
		return
	}

	hasFilters := filterQ != "" || filterStatus != ""
	rows := buildWorkflowRows(workflows, filterQ, filterStatus)
	var empty *emptyState
	if len(rows) == 0 {
		empty = workflowsEmptyState(hasFilters, c.deps.ReadOnly)
	}
	fv := workflowsListView{
		Rows:         rows,
		FilterQ:      filterQ,
		FilterStatus: filterStatus,
		HasFilters:   hasFilters,
		Empty:        empty,
		ReadOnly:     c.deps.ReadOnly,
	}
	html, err := renderFragment(c.ts.base, "workflows-tbody", fv)
	if err != nil {
		c.deps.Logger.Error("console: render workflows-tbody fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// workflowsEmptyState returns the appropriate empty-state binding depending
// on whether filters produced zero results (vs. no workflows at all).
func workflowsEmptyState(filtered bool, readOnly bool) *emptyState {
	if filtered {
		return &emptyState{
			Icon:        "⊘",
			Title:       "No matches",
			Description: "No workflows match your current filters.",
		}
	}
	es := &emptyState{
		Icon:        "◎",
		Title:       "No workflows yet",
		Description: "Create your first workflow to start sending notifications.",
		ReadOnly:    readOnly,
	}
	if !readOnly {
		es.PrimaryAction = &emptyStateAction{
			Label: "Create workflow",
			Href:  "/console/workflows/new",
		}
	}
	return es
}

// serveWorkflowDetail renders GET /console/workflows/{slug} (read-only).
func (c *Console) serveWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowDetail: w is nil")
	}
	if r == nil {
		panic("serveWorkflowDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowDetail: no scope in context (route not behind sessionMiddleware)")
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
		c.deps.Logger.Error("console: get workflow", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	steps, err := decodeStepsForDetail(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: decode workflow steps", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	schemaJSON := prettyJSON(wf.PayloadSchema)

	statusLabel := "inactive"
	if wf.Active {
		statusLabel = "active"
	}
	header := pageHeader{
		Title:    wf.Name,
		Subtitle: fmt.Sprintf("Trigger identifier: %s", wf.TriggerIdentifier),
		Tiles: []tile{
			{Label: statusLabel, Count: 0, Tone: workflowStatusTone(wf.Active)},
		},
	}
	if err := header.validate(); err != nil {
		c.deps.Logger.Error("console: workflow detail header", "slug", slug, "err", err)
		header = pageHeader{Title: wf.Name}
	}

	view := workflowDetailView{
		Header:            header,
		ID:                wf.ID,
		Name:              wf.Name,
		TriggerIdentifier: wf.TriggerIdentifier,
		Active:            wf.Active,
		Steps:             steps,
		PayloadSchemaJSON: schemaJSON,
		ReadOnly:          true, // detail is always read-only in Wave 1
	}
	c.renderPage(w, r, http.StatusOK, "workflow-detail", wf.Name, view)
}

// decodeStepsForDetail unmarshals the steps JSON into the detail view type,
// pretty-printing each step's controlValues for legible display.
func decodeStepsForDetail(raw json.RawMessage) ([]workflowDetailStepView, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var steps []domain.Step
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	out := make([]workflowDetailStepView, 0, len(steps))
	for _, s := range steps {
		out = append(out, workflowDetailStepView{
			StepID:            s.StepID,
			Type:              s.Type,
			Name:              s.Name,
			ControlValuesJSON: prettyJSON(s.ControlValues),
		})
	}
	return out, nil
}

// prettyJSON returns a pretty-printed JSON string from raw, or "" when raw
// is empty or not valid JSON. Never returns an error to the caller —
// rendering a blank is better than a 500 for a cosmetic field.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// workflowStatusTone maps workflow active bool to a tile tone.
func workflowStatusTone(active bool) tileTone {
	if active {
		return toneSuccess
	}
	return toneWarning
}

// isNotFound reports whether err is a store not-found sentinel.
func isNotFound(err error) bool {
	return err == store.ErrNotFound
}
