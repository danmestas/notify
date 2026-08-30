package console

import (
	"context"
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
	"github.com/danmestas/notify/internal/steptype"
)

// workflow_builder.go renders the full workflow editor page (W2-A4) and its
// step CRUD handlers:
//
//   - GET  /console/workflows/{slug}                     editor page
//   - POST /console/api/workflows/{slug}/save             rename + payload schema
//   - POST /console/api/workflows/{slug}/steps            add step
//   - POST /console/api/workflows/{slug}/steps/reorder    move step up/down
//   - POST /console/api/workflows/{slug}/steps/{id}/delete remove step
//   - POST /console/api/workflows/{slug}/steps/{id}/save   step config update
//   - GET  /console/api/fragments/workflow-step-editor/{slug}/{id}
//
// CONDITIONS row authoring and the live Handlebars preview live in
// workflow_step_conditions.go; the fire-test page lives in
// workflow_fire_test_page.go.
//
// Every mutation handler is gated on Deps.ReadOnly server-side and
// answers a native form POST with a 303 redirect (no SSE).

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
	// StepCatalog is the ordered list of step types from the registry, used to
	// render the add-step button menu. Driven from steptype.Default().Catalog()
	// so all registered types appear without manual list maintenance.
	StepCatalog []steptype.StepEntry
	// AIConfigured reports whether the AI workflow builder is wired, gating the
	// "Refine with AI" copilot sidebar (B4). When false the sidebar shows a
	// not-configured note instead of the prompt form.
	AIConfigured bool
	// AIBuilderSuggestions are clickable example-prompt cards shown in the
	// Refine-with-AI sidebar to help users get started. Each string is a
	// complete example prompt; clicking a card prefills the prompt textarea.
	AIBuilderSuggestions []string
	// Critical marks a no-opt-out workflow (mirrors domain.Workflow.Critical):
	// subscriber channel/workflow preferences cannot suppress delivery.
	Critical bool
	// Versions are the workflow's published version history, newest first,
	// capped at the newest 20. Empty when Deps.WorkflowVersions is nil or the
	// workflow has never been published.
	Versions []workflowVersionRow
	// VersionsAvailable gates the version-history panel: true when
	// Deps.WorkflowVersions is wired (nil is legal — it hides the panel
	// rather than 500ing).
	VersionsAvailable bool
	// ConfirmRollback names the version id awaiting inline rollback confirm
	// (?confirmRollback=<id>), mirroring ConfirmDelete's two-step pattern.
	ConfirmRollback string
	// ConfirmDeleteStep names the step id awaiting inline delete confirm
	// (?confirmDeleteStep=<id>). Per-step rather than a page-level flag: a
	// workflow has several steps, and a shared flag would let a confirm raised
	// for one step delete another.
	ConfirmDeleteStep string
	// VersionsError reports that Deps.WorkflowVersions.ListByWorkflow failed
	// (logged, non-fatal). Distinguishes a backend error from a genuinely
	// empty version history so the panel doesn't show "No versions published
	// yet." for a broken load.
	VersionsError bool
}

// workflowVersionRow is one row in the version-history panel.
type workflowVersionRow struct {
	ID string
	// Seq is the version's sequence number ("v{{Seq}}").
	Seq int
	// RelTime is the relativeTime-formatted publish timestamp.
	RelTime string
	// Current marks the newest (highest-seq) version — it renders a chip
	// instead of a rollback link.
	Current bool
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
	// EditorPartial is the html/template partial name rendering this step
	// type's editor fields, supplied by the step-type registry. The editor
	// template dispatches to it via the "partial" helper instead of an
	// inline per-type if/else chain.
	EditorPartial string
	// Combinator is the AND/OR combinator of the step's first filter Group,
	// pre-filling the conditions fieldset (defaults to "AND").
	Combinator string
	// Conditions are the step's filter conditions (the first Group's
	// Conditions), flattened for the per-row form pre-fill. The slice is
	// padded to stepConditionRows so the editor renders a fixed pool of rows;
	// rows beyond VisibleConditions are hidden client-side until "Add
	// condition" reveals them.
	Conditions []stepConditionRow
	// VisibleConditions seeds the $condRowCount signal: how many condition
	// rows are shown on load. It is the count of saved (non-blank) conditions,
	// clamped to at least 1 so an unconfigured step still shows one empty row
	// to author into.
	VisibleConditions int
	// IntegrationOptions is the "Integration" select's option list for the
	// four send step types (email/sms/push/chat): a leading "" → "Default
	// (primary)" entry followed by the scope's ACTIVE integrations of that
	// channel, labeled "identifier — provider label". Left nil for every
	// other step type, which hides the select (issue #314 stage 3).
	IntegrationOptions []uiSelectOption
}

// workflowStepEditorView is what the workflow-step-editor fragment binds.
type workflowStepEditorView struct {
	// Step is nil when no step is selected / found.
	Step     *workflowStepEditorStep
	Slug     string
	CSRF     string
	ReadOnly bool
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

	versionRows, versionsAvailable, versionsError := c.buildWorkflowVersionRows(r.Context(), scope, slug, wf.ID)

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
		Header:               header,
		ID:                   wf.ID,
		Name:                 wf.Name,
		TriggerIdentifier:    wf.TriggerIdentifier,
		Active:               wf.Active,
		Steps:                steps,
		PayloadSchemaJSON:    schemaJSON,
		ReadOnly:             c.deps.ReadOnly,
		ConfirmDelete:        r.URL.Query().Get("confirmDelete") == "true",
		FormError:            r.URL.Query().Get("error"),
		ActiveTab:            "editor",
		Signals:              workflowsSignalsJSON(false),
		StepCatalog:          c.stepTypes.Catalog(),
		AIConfigured:         c.aiBuilder != nil,
		AIBuilderSuggestions: aiBuilderRefineSuggestions,
		Critical:             wf.Critical,
		Versions:             versionRows,
		VersionsAvailable:    versionsAvailable,
		ConfirmRollback:      r.URL.Query().Get("confirmRollback"),
		ConfirmDeleteStep:    r.URL.Query().Get("confirmDeleteStep"),
		VersionsError:        versionsError,
	}
	c.renderPage(w, r, http.StatusOK, "workflow-detail", wf.Name, view)
}

// buildWorkflowVersionRows loads the workflow's version history (newest
// first, capped at the newest 20) for the detail page's panel. It is
// non-fatal: a nil Deps.WorkflowVersions or a list error logs and returns an
// empty panel rather than 500ing the whole page — the version history is a
// secondary view, not load-bearing for the editor. The third return value
// distinguishes a ListByWorkflow error from a genuinely empty history so the
// template can render a distinct message instead of the empty-state copy.
func (c *Console) buildWorkflowVersionRows(ctx context.Context, scope domain.Scope, slug, workflowID string) ([]workflowVersionRow, bool, bool) {
	if c.deps.WorkflowVersions == nil {
		return nil, false, false
	}
	vers, err := c.deps.WorkflowVersions.ListByWorkflow(ctx, scope, workflowID)
	if err != nil {
		c.deps.Logger.Error("console: list workflow versions", "slug", slug, "err", err)
		return nil, true, true
	}
	// ListByWorkflow returns seq-ascending (oldest first); cap to the newest 20.
	if len(vers) > 20 {
		vers = vers[len(vers)-20:]
	}
	rows := make([]workflowVersionRow, 0, len(vers))
	for i := len(vers) - 1; i >= 0; i-- {
		v := vers[i]
		rows = append(rows, workflowVersionRow{
			ID:      v.ID,
			Seq:     v.Seq,
			RelTime: relativeTime(v.CreatedAt),
			Current: i == len(vers)-1,
		})
	}
	return rows, true, false
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

	critical := r.FormValue("critical") == "1" || r.FormValue("critical") == "on"

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          name,
		Steps:         steps,
		PayloadSchema: schema,
		Critical:      critical,
	}); err != nil {
		// ErrNoSteps means the config write above already succeeded — Update
		// only skipped re-registering the (0-step) workflow with the engine.
		// A blanket "Save failed." would be false: the operator's edits are
		// on the row. Say so accurately instead (M6).
		if errors.Is(err, authoring.ErrNoSteps) {
			http.Redirect(w, r, editURL+"?error="+
				url.QueryEscape("Configuration saved, but the workflow needs at least one step to publish."), http.StatusSeeOther)
			return
		}
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
	if !c.stepTypes.Has(stepType) {
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
		ControlValues: c.stepTypes.Get(stepType).DefaultControlValues(),
	})

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          wf.Name,
		Steps:         steps,
		PayloadSchema: wf.PayloadSchema,
		Critical:      wf.Critical,
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
		Critical:      wf.Critical,
	}); err != nil && !errors.Is(err, authoring.ErrNoSteps) {
		// ErrNoSteps is expected and non-fatal here: deleting the last
		// remaining step legitimately leaves zero steps, and the row is
		// already persisted — Update only skipped the (impossible) engine
		// re-registration.
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
		Critical:      wf.Critical,
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
		cv, cvErr := c.controlValuesFromForm(steps[i].Type, r)
		if cvErr != nil {
			http.Redirect(w, r, editURL+"?error="+url.QueryEscape(cvErr.Error()), http.StatusSeeOther)
			return
		}
		steps[i].ControlValues = cv
		filters, fErr := conditionsFromForm(r)
		if fErr != nil {
			http.Redirect(w, r, editURL+"?error="+url.QueryEscape(fErr.Error()), http.StatusSeeOther)
			return
		}
		steps[i].Filters = filters
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
		Critical:      wf.Critical,
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
			combinator, conditions, visible := conditionsFromFilters(st.Filters)
			// Drive the per-type field markup and the controlValues flattening
			// from the step-type registry. Unknown/legacy types fall back to the
			// generic JSON-textarea partial and generic flatten (the old default
			// editor arm) so a step persisted under a retired type still renders.
			cmap := controlMap(st.ControlValues)
			partial := "wf-step-fields-default"
			if stype := c.stepTypes.Get(st.Type); stype != nil {
				cmap = stype.ControlMap(st.ControlValues)
				partial = stype.EditorPartial()
			}
			view.Step = &workflowStepEditorStep{
				StepID:            st.StepID,
				Type:              st.Type,
				Name:              st.Name,
				ControlValuesJSON: prettyJSON(st.ControlValues),
				ControlMap:        cmap,
				EditorPartial:     partial,
				Combinator:        combinator,
				Conditions:        conditions,
				VisibleConditions: visible,
			}
			if sendIntegrationChannels[st.Type] {
				view.Step.IntegrationOptions = c.integrationOptions(r.Context(), scope, st.Type)
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

// sendIntegrationChannels is the set of send step types carrying the
// optional "integration" control value (issue #314): email, sms, push, chat.
// in_app/stream/delay/digest/throttle/http_request never resolve through an
// integration, so they never get the select.
var sendIntegrationChannels = map[string]bool{
	"email": true,
	"sms":   true,
	"push":  true,
	"chat":  true,
}

// integrationOptions builds the "Integration" select's option list for
// channel: a leading "" → "Default (primary)" entry followed by the scope's
// ACTIVE integrations on that channel, ordered as store.IntegrationRepo.List
// returns them (primary desc, priority asc, id asc). A nil Deps.Integrations
// or a list error degrades to just the default entry rather than 500ing the
// step editor over a cosmetic dropdown.
func (c *Console) integrationOptions(ctx context.Context, scope domain.Scope, channel string) []uiSelectOption {
	opts := []uiSelectOption{{Value: "", Label: "Default (primary)"}}
	if c.deps.Integrations == nil {
		return opts
	}
	all, err := c.deps.Integrations.List(ctx, scope)
	if err != nil {
		c.deps.Logger.Error("console: list integrations for step editor", "channel", channel, "err", err)
		return opts
	}
	for _, in := range all {
		if in.Channel != channel || !in.Active {
			continue
		}
		opts = append(opts, uiSelectOption{
			Value: in.Identifier,
			Label: in.Identifier + " — " + providerLabel(in.ProviderID),
		})
	}
	return opts
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

// controlValuesFromForm builds a step's controlValues JSON from the per-type
// form fields posted by the step editor, delegating to the step-type registry.
// An unknown/legacy type falls back to the free-form control_values_json
// textarea (the old default editor arm) so an editor save for a retired type
// still round-trips.
func (c *Console) controlValuesFromForm(stepType string, r *http.Request) (json.RawMessage, error) {
	if st := c.stepTypes.Get(stepType); st != nil {
		return st.ParseForm(r.Form)
	}
	raw := strings.TrimSpace(r.FormValue("control_values_json"))
	if raw == "" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("control values must be valid JSON")
	}
	return json.RawMessage(raw), nil
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
