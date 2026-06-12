package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// workflows.go renders the workflows list, the create + duplicate
// side-sheets (W2-A3), and the full workflow editor (W2-A4):
//
//   - GET  /console/workflows                            list page
//   - GET  /console/api/fragments/workflows-list         list tbody fragment
//   - GET  /console/workflows/new                        create slide-over
//   - GET  /console/workflows/{slug}                     editor page
//   - GET  /console/workflows/{slug}/duplicate           duplicate slide-over
//   - POST /console/api/workflows                         create
//   - POST /console/api/workflows/{slug}/duplicate        duplicate
//   - POST /console/api/workflows/{slug}/save             rename + payload schema
//   - POST /console/api/workflows/{slug}/delete           delete
//   - POST /console/api/workflows/{slug}/activate         active toggle
//   - POST /console/api/workflows/{slug}/steps            add step
//   - POST /console/api/workflows/{slug}/steps/reorder    move step up/down
//   - POST /console/api/workflows/{slug}/steps/{id}/delete remove step
//   - POST /console/api/workflows/{slug}/steps/{id}/save   step config update
//   - GET  /console/api/fragments/workflow-step-editor/{slug}/{id}
//
// Every mutation handler is gated on Deps.ReadOnly server-side and
// answers a native form POST with a 303 redirect (no SSE).

// workflowStepTypes is the closed set of step types the editor offers as
// "add step" buttons. delay/digest are sleep-compiled; the rest are channels.
var workflowStepTypes = []string{"in_app", "email", "push", "sms", "delay", "digest"}

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
	// ReadOnly hides the row's mutation actions (Edit/Dup/Delete).
	ReadOnly bool
	// ConfirmDelete is true when this row is the one awaiting inline
	// two-step delete confirmation.
	ConfirmDelete bool
	// CSRF is the session token for the inline confirm-delete POST form.
	CSRF string
}

// workflowsListView is the #workflows-tbody fragment binding.
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
	// ConfirmDelete names the row TriggerIdentifier awaiting inline two-step
	// delete confirmation (chassis idiom — no native confirm() dialogs).
	ConfirmDelete string
	// CSRF is the session token for confirm-delete forms rendered in the fragment.
	CSRF string
}

// workflowsPageView is what workflows.html binds (list page + slide-overs).
type workflowsPageView struct {
	Header   pageHeader
	Fragment workflowsListView
	Signals  template.JS
	ReadOnly bool
	// ShowCreate renders the create slide-over (/console/workflows/new).
	ShowCreate bool
	// ShowDuplicate renders the duplicate slide-over
	// (/console/workflows/{slug}/duplicate).
	ShowDuplicate bool
	// FormError is an inline validation error surfaced above the form.
	FormError string
	// Source workflow info for the duplicate slide-over pre-fill.
	SourceName       string
	SourceSlug       string
	SuggestedName    string
	SuggestedTrigger string
}

// workflowDetailStepView is one step entry in the editor's step list.
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

// workflowEditorView is what workflow_detail.html binds in Wave 2.
type workflowEditorView struct {
	Header            pageHeader
	ID                string
	Name              string
	TriggerIdentifier string
	Active            bool
	Steps             []workflowDetailStepView
	// PayloadSchemaJSON is the pretty-printed payload schema (empty when absent).
	PayloadSchemaJSON string
	ReadOnly          bool
	// ConfirmDelete shows the inline two-step delete confirm (?confirmDelete=true).
	ConfirmDelete bool
	// FormError is an inline validation error (?error=...).
	FormError string
	// ActiveTab is the selected tab ("editor" | "activity"); reserved for a
	// future activity tab.
	ActiveTab string
	Signals   template.JS
}

// workflowStepEditorStep carries one step's decoded config for the
// per-type step editor fragment.
type workflowStepEditorStep struct {
	StepID            string
	Type              string
	Name              string
	ControlValuesJSON string
	// ControlMap is the decoded controlValues flattened to strings for
	// form-field pre-fill.
	ControlMap map[string]string
}

// workflowStepEditorView is what the workflow-step-editor fragment binds.
type workflowStepEditorView struct {
	// Step is nil when no step is selected / found.
	Step     *workflowStepEditorStep
	Slug     string
	CSRF     string
	ReadOnly bool
}

// workflowsSignalsJSON returns the editor/list page's Datastar signals.
func workflowsSignalsJSON() template.JS {
	b, err := json.Marshal(map[string]string{"wfTab": "editor"})
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
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

// buildWorkflowsListView lists workflows for the current scope and applies the
// confirm-delete + CSRF + read-only decoration each row needs.
func (c *Console) buildWorkflowsListView(r *http.Request, q, status string) (workflowsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildWorkflowsListView: no scope in context")
	}
	workflows, err := c.deps.Authoring.List(r.Context(), scope)
	if err != nil {
		return workflowsListView{}, err
	}
	hasFilters := q != "" || status != ""
	rows := buildWorkflowRows(workflows, q, status)

	s, _ := sessionFromContext(r.Context())
	confirmDelete := r.URL.Query().Get("confirmDelete")
	for i := range rows {
		rows[i].ReadOnly = c.deps.ReadOnly
		rows[i].CSRF = s.CSRF
		rows[i].ConfirmDelete = confirmDelete != "" && rows[i].TriggerIdentifier == confirmDelete
	}

	var empty *emptyState
	if len(rows) == 0 {
		empty = workflowsEmptyState(hasFilters, c.deps.ReadOnly)
	}
	return workflowsListView{
		Rows:          rows,
		FilterQ:       q,
		FilterStatus:  status,
		HasFilters:    hasFilters,
		Empty:         empty,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: confirmDelete,
		CSRF:          s.CSRF,
	}, nil
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

	frag, err := c.buildWorkflowsListView(r, "", "")
	if err != nil {
		c.deps.Logger.Error("console: workflows list view", "err", err)
		c.serveError(w, r)
		return
	}
	view := workflowsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  workflowsSignalsJSON(),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "workflows", "Workflows", view)
}

// serveWorkflowNew renders GET /console/workflows/new (create slide-over over
// the list). An ?error= query param surfaces inline (identifier conflict).
func (c *Console) serveWorkflowNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowNew: w is nil")
	}
	if r == nil {
		panic("serveWorkflowNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveWorkflowNew: no scope in context")
	}
	header := pageHeader{Title: "Workflows", Subtitle: "All notification workflows in this environment."}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Workflows"}
	}
	frag, err := c.buildWorkflowsListView(r, "", "")
	if err != nil {
		c.deps.Logger.Error("console: workflow new view", "err", err)
		c.serveError(w, r)
		return
	}
	view := workflowsPageView{
		Header:     header,
		Fragment:   frag,
		Signals:    workflowsSignalsJSON(),
		ReadOnly:   c.deps.ReadOnly,
		ShowCreate: !c.deps.ReadOnly,
		FormError:  r.URL.Query().Get("error"),
	}
	c.renderPage(w, r, http.StatusOK, "workflows", "Create workflow", view)
}

// serveWorkflowDuplicate renders GET /console/workflows/{slug}/duplicate.
func (c *Console) serveWorkflowDuplicate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowDuplicate: w is nil")
	}
	if r == nil {
		panic("serveWorkflowDuplicate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowDuplicate: no scope in context")
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		c.serveNotFound(w, r)
		return
	}
	src, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: duplicate source lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	header := pageHeader{Title: "Workflows", Subtitle: "All notification workflows in this environment."}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Workflows"}
	}
	frag, err := c.buildWorkflowsListView(r, "", "")
	if err != nil {
		c.deps.Logger.Error("console: duplicate view", "err", err)
		c.serveError(w, r)
		return
	}
	view := workflowsPageView{
		Header:           header,
		Fragment:         frag,
		Signals:          workflowsSignalsJSON(),
		ReadOnly:         c.deps.ReadOnly,
		ShowDuplicate:    !c.deps.ReadOnly,
		FormError:        r.URL.Query().Get("error"),
		SourceName:       src.Name,
		SourceSlug:       src.TriggerIdentifier,
		SuggestedName:    "Copy of " + src.Name,
		SuggestedTrigger: src.TriggerIdentifier + "-copy",
	}
	c.renderPage(w, r, http.StatusOK, "workflows", "Duplicate workflow", view)
}

// serveFragmentWorkflowsList answers GET /console/api/fragments/workflows-list.
func (c *Console) serveFragmentWorkflowsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentWorkflowsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentWorkflowsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
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

	fv, err := c.buildWorkflowsListView(r, filterQ, filterStatus)
	if err != nil {
		c.deps.Logger.Error("console: fragment list workflows", "err", err)
		http.Error(w, "list workflows failed", http.StatusInternalServerError)
		return
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

// serveWorkflowDetail renders GET /console/workflows/{slug} (Wave 2 editor).
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

	view := workflowEditorView{
		Header:            header,
		ID:                wf.ID,
		Name:              wf.Name,
		TriggerIdentifier: wf.TriggerIdentifier,
		Active:            wf.Active,
		Steps:             steps,
		PayloadSchemaJSON: schemaJSON,
		ReadOnly:          c.deps.ReadOnly,
		ConfirmDelete:     r.URL.Query().Get("confirmDelete") == "true",
		FormError:         r.URL.Query().Get("error"),
		ActiveTab:         "editor",
		Signals:           workflowsSignalsJSON(),
	}
	c.renderPage(w, r, http.StatusOK, "workflow-detail", wf.Name, view)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleWorkflowCreate handles POST /console/api/workflows.
func (c *Console) handleWorkflowCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowCreate: w is nil")
	}
	if r == nil {
		panic("handleWorkflowCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowCreate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	trigger := strings.TrimSpace(r.FormValue("trigger_identifier"))
	if name == "" || trigger == "" {
		http.Redirect(w, r, "/console/workflows/new?error="+
			url.QueryEscape("Name and trigger identifier are required."), http.StatusSeeOther)
		return
	}

	// Reject a duplicate identifier before writing — surface it inline.
	if _, err := c.deps.Authoring.GetBySlug(r.Context(), scope, trigger); err == nil {
		http.Redirect(w, r, "/console/workflows/new?error="+
			url.QueryEscape("Trigger identifier already exists."), http.StatusSeeOther)
		return
	} else if !isNotFound(err) {
		c.deps.Logger.Error("console: create workflow identifier check", "err", err)
		c.serveError(w, r)
		return
	}

	wf, err := c.deps.Authoring.Create(r.Context(), scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
	})
	if err != nil {
		c.deps.Logger.Error("console: create workflow", "err", err)
		http.Redirect(w, r, "/console/workflows/new?error="+
			url.QueryEscape("Create failed."), http.StatusSeeOther)
		return
	}
	if err := c.deps.Authoring.Publish(r.Context(), scope, wf.TriggerIdentifier); err != nil {
		c.deps.Logger.Error("console: publish new workflow", "err", err)
		// The row exists; the editor is still reachable. Send the operator there.
	}
	http.Redirect(w, r, "/console/workflows/"+wf.TriggerIdentifier, http.StatusSeeOther)
}

// handleWorkflowDuplicate handles POST /console/api/workflows/{slug}/duplicate.
func (c *Console) handleWorkflowDuplicate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowDuplicate: w is nil")
	}
	if r == nil {
		panic("handleWorkflowDuplicate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowDuplicate: no scope in context")
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
	name := strings.TrimSpace(r.FormValue("name"))
	trigger := strings.TrimSpace(r.FormValue("trigger_identifier"))
	dupURL := "/console/workflows/" + slug + "/duplicate"
	if name == "" || trigger == "" {
		http.Redirect(w, r, dupURL+"?error="+
			url.QueryEscape("Name and trigger identifier are required."), http.StatusSeeOther)
		return
	}

	created, err := c.deps.Authoring.Duplicate(r.Context(), scope, slug, authoring.DuplicateInput{
		Name:              name,
		TriggerIdentifier: trigger,
	})
	if err != nil {
		if errors.Is(err, authoring.ErrIdentifierConflict) {
			http.Redirect(w, r, dupURL+"?error="+
				url.QueryEscape("Trigger identifier already exists."), http.StatusSeeOther)
			return
		}
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: duplicate workflow", "slug", slug, "err", err)
		http.Redirect(w, r, dupURL+"?error="+
			url.QueryEscape("Duplicate failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/console/workflows/"+created.TriggerIdentifier, http.StatusSeeOther)
}

// handleWorkflowSave handles POST /console/api/workflows/{slug}/save:
// renames the workflow and updates its payload schema, preserving steps.
func (c *Console) handleWorkflowSave(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowSave: w is nil")
	}
	if r == nil {
		panic("handleWorkflowSave: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowSave: no scope in context")
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
	name := strings.TrimSpace(r.FormValue("name"))
	editURL := "/console/workflows/" + slug
	if name == "" {
		http.Redirect(w, r, editURL+"?error="+
			url.QueryEscape("Name is required."), http.StatusSeeOther)
		return
	}

	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: save workflow lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: save workflow decode steps", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	var schema json.RawMessage
	if raw := strings.TrimSpace(r.FormValue("payload_schema")); raw != "" {
		if !json.Valid([]byte(raw)) {
			http.Redirect(w, r, editURL+"?error="+
				url.QueryEscape("Payload schema is not valid JSON."), http.StatusSeeOther)
			return
		}
		schema = json.RawMessage(raw)
	}

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          name,
		Steps:         steps,
		PayloadSchema: schema,
	}); err != nil {
		var compileErr *authoring.ErrCompileFailure
		if errors.As(err, &compileErr) {
			http.Redirect(w, r, editURL+"?error="+
				url.QueryEscape("Step configuration is invalid: "+compileErr.Error()), http.StatusSeeOther)
			return
		}
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: save workflow", "slug", slug, "err", err)
		http.Redirect(w, r, editURL+"?error="+
			url.QueryEscape("Save failed."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, editURL, http.StatusSeeOther)
}

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

// handleWorkflowStepAdd handles POST /console/api/workflows/{slug}/steps.
func (c *Console) handleWorkflowStepAdd(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowStepAdd: w is nil")
	}
	if r == nil {
		panic("handleWorkflowStepAdd: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowStepAdd: no scope in context")
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
	stepType := strings.TrimSpace(r.FormValue("type"))
	if !isKnownStepType(stepType) {
		http.Error(w, "unknown step type", http.StatusBadRequest)
		return
	}

	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: step add lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: step add decode", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	steps = append(steps, domain.Step{
		StepID:        nextStepID(steps, stepType),
		Type:          stepType,
		ControlValues: defaultControlValues(stepType),
	})

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          wf.Name,
		Steps:         steps,
		PayloadSchema: wf.PayloadSchema,
	}); err != nil {
		var compileErr *authoring.ErrCompileFailure
		if errors.As(err, &compileErr) {
			http.Redirect(w, r, "/console/workflows/"+slug+"?error="+
				url.QueryEscape(compileErr.Error()), http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: step add update", "slug", slug, "err", err)
		http.Error(w, "add step failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/workflows/"+slug, http.StatusSeeOther)
}

// handleWorkflowStepDelete handles POST /console/api/workflows/{slug}/steps/{stepId}/delete.
func (c *Console) handleWorkflowStepDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowStepDelete: w is nil")
	}
	if r == nil {
		panic("handleWorkflowStepDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowStepDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	stepID := chi.URLParam(r, "stepId")
	if slug == "" || stepID == "" {
		http.Error(w, "missing slug or stepId", http.StatusBadRequest)
		return
	}

	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: step delete lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: step delete decode", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	out := steps[:0]
	for _, s := range steps {
		if s.StepID == stepID {
			continue
		}
		out = append(out, s)
	}

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          wf.Name,
		Steps:         out,
		PayloadSchema: wf.PayloadSchema,
	}); err != nil {
		c.deps.Logger.Error("console: step delete update", "slug", slug, "err", err)
		http.Error(w, "delete step failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/workflows/"+slug, http.StatusSeeOther)
}

// handleWorkflowStepReorder handles POST /console/api/workflows/{slug}/steps/reorder.
// Swaps the named step with its up/down neighbour.
func (c *Console) handleWorkflowStepReorder(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowStepReorder: w is nil")
	}
	if r == nil {
		panic("handleWorkflowStepReorder: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowStepReorder: no scope in context")
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
	stepID := strings.TrimSpace(r.FormValue("stepId"))
	direction := strings.TrimSpace(r.FormValue("direction"))
	if stepID == "" || (direction != "up" && direction != "down") {
		http.Error(w, "missing stepId or direction", http.StatusBadRequest)
		return
	}

	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: step reorder lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: step reorder decode", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	idx := -1
	for i, s := range steps {
		if s.StepID == stepID {
			idx = i
			break
		}
	}
	if idx == -1 {
		http.Redirect(w, r, "/console/workflows/"+slug, http.StatusSeeOther)
		return
	}
	swap := idx + 1
	if direction == "up" {
		swap = idx - 1
	}
	if swap >= 0 && swap < len(steps) {
		steps[idx], steps[swap] = steps[swap], steps[idx]
	}

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          wf.Name,
		Steps:         steps,
		PayloadSchema: wf.PayloadSchema,
	}); err != nil {
		c.deps.Logger.Error("console: step reorder update", "slug", slug, "err", err)
		http.Error(w, "reorder failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/workflows/"+slug, http.StatusSeeOther)
}

// handleWorkflowStepSave handles POST /console/api/workflows/{slug}/steps/{stepId}/save.
// It rebuilds the step's controlValues from per-type form fields.
func (c *Console) handleWorkflowStepSave(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowStepSave: w is nil")
	}
	if r == nil {
		panic("handleWorkflowStepSave: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowStepSave: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	stepID := chi.URLParam(r, "stepId")
	if slug == "" || stepID == "" {
		http.Error(w, "missing slug or stepId", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: step save lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: step save decode", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	editURL := "/console/workflows/" + slug
	found := false
	for i := range steps {
		if steps[i].StepID != stepID {
			continue
		}
		found = true
		cv, cvErr := controlValuesFromForm(steps[i].Type, r)
		if cvErr != nil {
			http.Redirect(w, r, editURL+"?error="+url.QueryEscape(cvErr.Error()), http.StatusSeeOther)
			return
		}
		steps[i].ControlValues = cv
		break
	}
	if !found {
		c.serveNotFound(w, r)
		return
	}

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          wf.Name,
		Steps:         steps,
		PayloadSchema: wf.PayloadSchema,
	}); err != nil {
		var compileErr *authoring.ErrCompileFailure
		if errors.As(err, &compileErr) {
			http.Redirect(w, r, editURL+"?error="+url.QueryEscape(compileErr.Error()), http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: step save update", "slug", slug, "err", err)
		http.Error(w, "save step failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, editURL, http.StatusSeeOther)
}

// serveFragmentWorkflowStepEditor answers
// GET /console/api/fragments/workflow-step-editor/{slug}/{stepId}.
func (c *Console) serveFragmentWorkflowStepEditor(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentWorkflowStepEditor: w is nil")
	}
	if r == nil {
		panic("serveFragmentWorkflowStepEditor: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentWorkflowStepEditor: no scope in context")
	}
	slug := chi.URLParam(r, "slug")
	stepID := chi.URLParam(r, "stepId")
	if slug == "" || stepID == "" {
		http.Error(w, "missing slug or stepId", http.StatusBadRequest)
		return
	}

	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: step editor lookup", "slug", slug, "err", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	steps, err := decodeSteps(wf.Steps)
	if err != nil {
		c.deps.Logger.Error("console: step editor decode", "slug", slug, "err", err)
		http.Error(w, "decode failed", http.StatusInternalServerError)
		return
	}

	s, _ := sessionFromContext(r.Context())
	view := workflowStepEditorView{
		Slug:     slug,
		CSRF:     s.CSRF,
		ReadOnly: c.deps.ReadOnly,
	}
	for _, st := range steps {
		if st.StepID == stepID {
			st := st
			view.Step = &workflowStepEditorStep{
				StepID:            st.StepID,
				Type:              st.Type,
				Name:              st.Name,
				ControlValuesJSON: prettyJSON(st.ControlValues),
				ControlMap:        controlMap(st.ControlValues),
			}
			break
		}
	}

	html, err := renderFragment(c.ts.base, "workflow-step-editor", view)
	if err != nil {
		c.deps.Logger.Error("console: render step editor fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// isKnownStepType reports whether t is one of the offered step types.
func isKnownStepType(t string) bool {
	for _, k := range workflowStepTypes {
		if k == t {
			return true
		}
	}
	return false
}

// nextStepID generates a "{type}-{n}" id where n is one past the count of
// existing steps already carrying that type.
func nextStepID(steps []domain.Step, stepType string) string {
	n := 0
	for _, s := range steps {
		if s.Type == stepType {
			n++
		}
	}
	return fmt.Sprintf("%s-%d", stepType, n+1)
}

// defaultControlValues returns a minimal valid controlValues blob for a new
// step. delay/digest need a positive amount or they fail Compile, so they seed
// a 1-minute window; channel steps seed an empty object.
func defaultControlValues(stepType string) json.RawMessage {
	switch stepType {
	case "delay", "digest":
		return json.RawMessage(`{"amount":1,"unit":"minutes"}`)
	default:
		return json.RawMessage(`{}`)
	}
}

// controlValuesFromForm builds a step's controlValues JSON from the per-type
// form fields posted by the step editor.
func controlValuesFromForm(stepType string, r *http.Request) (json.RawMessage, error) {
	field := func(name string) string { return strings.TrimSpace(r.FormValue(name)) }
	var m map[string]any
	switch stepType {
	case "email", "in_app":
		m = map[string]any{"subject": field("subject"), "body": field("body")}
	case "push":
		m = map[string]any{"title": field("title"), "body": field("body")}
	case "sms":
		m = map[string]any{"body": field("body")}
	case "delay", "digest":
		amount, err := strconv.Atoi(field("amount"))
		if err != nil || amount < 1 {
			return nil, fmt.Errorf("amount must be a positive whole number")
		}
		unit := field("unit")
		if unit == "" {
			unit = "minutes"
		}
		m = map[string]any{"amount": amount, "unit": unit}
	default:
		raw := field("control_values_json")
		if raw == "" {
			return json.RawMessage(`{}`), nil
		}
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("control values must be valid JSON")
		}
		return json.RawMessage(raw), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal control values: %w", err)
	}
	return b, nil
}

// controlMap flattens a step's controlValues JSON into a string map for form
// pre-fill. Non-string scalar values are stringified; nested objects are
// dropped (the per-type forms only read flat scalar fields).
func controlMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(t)
		}
	}
	return out
}

// decodeSteps unmarshals the steps JSON into domain steps for re-application.
func decodeSteps(raw json.RawMessage) ([]domain.Step, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var steps []domain.Step
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	return steps, nil
}

// decodeStepsForDetail unmarshals the steps JSON into the detail view type,
// pretty-printing each step's controlValues for legible display.
func decodeStepsForDetail(raw json.RawMessage) ([]workflowDetailStepView, error) {
	steps, err := decodeSteps(raw)
	if err != nil {
		return nil, err
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
	return errors.Is(err, store.ErrNotFound)
}
