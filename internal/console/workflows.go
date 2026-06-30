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
	"time"

	"github.com/aymerick/raymond"
	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/delivery/filter"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
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
//   - GET  /console/api/fragments/step-preview            live Handlebars preview (W4-W3)
//
// Every mutation handler is gated on Deps.ReadOnly server-side and
// answers a native form POST with a 303 redirect (no SSE).

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
	// UpdatedAt is the workflow's creation time, decoded from the ULID-embedded
	// millisecond timestamp in wf.ID (the workflows table carries no
	// created_at/updated_at column). Rendered by the relTime template helper.
	UpdatedAt time.Time
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
	// Total is the count of all workflows before filtering. Combined with
	// len(Rows) it produces the "X of Y workflows" footer label.
	Total int
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
	// ShowDuplicate renders the duplicate slide-over
	// (/console/workflows/{slug}/duplicate).
	ShowDuplicate bool
	// FormError is an inline validation error surfaced above the form.
	FormError string
	// AIConfigured reports whether the AI workflow builder is wired (an
	// ANTHROPIC_API_KEY is set). When false the Guided create tab shows a
	// not-configured note instead of the prompt form.
	AIConfigured bool
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
	// StepCatalog is the ordered list of step types from the registry, used to
	// render the add-step button menu. Driven from steptype.Default().Catalog()
	// so all registered types appear without manual list maintenance.
	StepCatalog []steptype.StepEntry
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
}

// stepConditionRow is a single CONDITIONS authoring row, flattened from a
// filter.Condition for form pre-fill. Value is rendered verbatim into a text
// input; On/Operator seed their respective uiSelect value signals.
type stepConditionRow struct {
	On       string
	Field    string
	Operator string
	Value    string
}

// stepConditionRows is the size of the fixed pool of condition rows the editor
// renders server-side and the iteration cap conditionsFromForm reads up to. The
// editor shows the first $condRowCount of them; "Add condition" grows the
// visible count up to this pool size. No nested groups.
const stepConditionRows = 10

// condRowSignals are the four per-row condition signals the editor binds:
// condOn{i} / condField{i} / condOperator{i} / condValue{i}. The remove
// handler's positional shift moves all four up a slot so the visible rows stay
// a dense 1..count block.
var condRowSignals = [...]string{"condOn", "condField", "condOperator", "condValue"}

// condRemoveExpr builds the Datastar data-on:click expression for row k's
// remove (×) button. A naive "blank row k + decrement count" leaves a visible
// hole when k is a MIDDLE row: the count only hides the trailing slot, so the
// blanked middle row stays on screen and the wrong row collapses. Instead this
// does a POSITIONAL SHIFT — every later filled row's four signals move up one
// slot (row j gets row j+1's values), the trailing visible slot is cleared, and
// the count is decremented. The result is always a dense, hole-free 1..count
// block so the decrement correctly hides the now-empty trailing slot.
//
// The pool size is static, so the full shift chain is generated at render time;
// each step is guarded by $condRowCount so only the in-range rows shift:
//   - $condRowCount > j+1  → copy row j+1 into row j
//   - $condRowCount === j+1 → row j is the last visible row being vacated, blank it
//
// The j===k, count===k+1 branch covers removing the last row (blank in place).
func condRemoveExpr(k int) template.JS {
	var b strings.Builder
	for j := k; j < stepConditionRows-1; j++ {
		// Copy row j+1 → row j when a later row exists.
		copies := make([]string, 0, len(condRowSignals))
		blanks := make([]string, 0, len(condRowSignals))
		for _, sig := range condRowSignals {
			cur := fmt.Sprintf("$%s%d", sig, j)
			next := fmt.Sprintf("$%s%d", sig, j+1)
			copies = append(copies, cur+" = "+next)
			blanks = append(blanks, cur+" = ''")
		}
		// Guarded ternary: shift-up when more rows follow, else blank when this
		// is the last visible row, else no-op.
		fmt.Fprintf(
			&b,
			"$condRowCount > %d ? (%s) : ($condRowCount === %d ? (%s) : null); ",
			j+1, strings.Join(copies, ", "),
			j+1, strings.Join(blanks, ", "),
		)
	}
	// Decrement the visible-row count (floor at 1).
	b.WriteString("$condRowCount = ($condRowCount > 1 ? $condRowCount - 1 : 1)")
	return template.JS(b.String())
}

// workflowStepEditorView is what the workflow-step-editor fragment binds.
type workflowStepEditorView struct {
	// Step is nil when no step is selected / found.
	Step     *workflowStepEditorStep
	Slug     string
	CSRF     string
	ReadOnly bool
}

// stepPreviewView is what the step-preview fragment binds (W4-W3).
type stepPreviewView struct {
	// Output is the rendered Handlebars output on success.
	Output string
	// Error is a template compile/render error shown inline; never causes a 500.
	Error string
	// IsError is true when Error is set, so the template can choose the error style.
	IsError bool
}

// workflowTestView is what workflow_test.html binds (W2-A5).
type workflowTestView struct {
	Header            pageHeader
	TriggerIdentifier string
	Name              string
	Active            bool
	// StepCount is the number of steps; when >1 the #17 banner is shown.
	StepCount int
	// PayloadExample is a pre-filled JSON example derived from the payload
	// schema (or "{}" when absent). Pre-populates the textarea.
	PayloadExample string
	// FormError is an inline error surfaced after a bad POST (e.g. invalid
	// JSON payload).
	FormError string
	// TransactionID is surfaced after a successful test fire (from ?txid=).
	TransactionID string
	// RunID is the dagnats run id minted for the test fire's transaction
	// (empty when not yet available). Surfaced alongside RunURL so the operator
	// sees the durable run reference even when the deep-link base is unset
	// (W4 ADR-013, #55).
	RunID string
	// RunURL is the dagnats deep-link for the transaction's run (empty when
	// DagnatsConsoleURL is unset or the run id is not yet available).
	RunURL string
	// InactiveBlocked is true when the workflow is inactive — the POST is
	// blocked and the form shows an explanatory message.
	InactiveBlocked bool
	ReadOnly        bool
	Signals         template.JS
	CSRF            string
}

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

// workflowsSignalsJSON returns the editor/list page's Datastar signals.
// newOpen seeds workflowNewOpen, which drives the create slide-over's
// data-show. It MUST be a real JSON boolean (map[string]any) — the string
// "false" is truthy in JS and would leave the slide-over open.
func workflowsSignalsJSON(newOpen bool) template.JS {
	b, err := json.Marshal(map[string]any{
		"wfTab":           "editor",
		"workflowNewOpen": newOpen,
		// wfCreateMode toggles the create slide-over between the Manual form and
		// the Guided (AI builder) tab. Defaults to manual so the unchanged path
		// is the first thing an operator sees (B2).
		"wfCreateMode": "manual",
	})
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
			UpdatedAt:         ulidTime(wf.ID),
		})
	}
	return rows
}

// ulidTime decodes the 48-bit millisecond timestamp encoded in the first 10
// characters of a ULID (Crockford base32). Workflows carry no created_at
// column, so the list's "Updated" column derives its value from the id minted
// at create time. Returns the zero time for a malformed/short id so relTime
// renders an em dash rather than a bogus epoch.
func ulidTime(id string) time.Time {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	if len(id) < 10 {
		return time.Time{}
	}
	upper := strings.ToUpper(id[:10])
	var ms uint64
	for _, c := range upper {
		idx := strings.IndexRune(alpha, c)
		if idx < 0 {
			return time.Time{}
		}
		ms = ms*32 + uint64(idx)
	}
	return time.UnixMilli(int64(ms))
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

	// Read confirmDelete from the URL query string (page loads and Cancel
	// redirect links) or, as a fallback, from the wfConfirmDelete signal
	// (Datastar @get requests where literal query params are dropped by the
	// runtime — the delete kebab sets this signal before calling @get).
	confirmDelete := r.URL.Query().Get("confirmDelete")
	if confirmDelete == "" {
		merged := mergeFragmentParams(r, "wfConfirmDelete")
		if vs := merged["confirmDelete"]; len(vs) > 0 {
			confirmDelete = vs[0]
		}
	}

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
		Total:         len(workflows),
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
		Header:       header,
		Fragment:     frag,
		Signals:      workflowsSignalsJSON(false),
		ReadOnly:     c.deps.ReadOnly,
		AIConfigured: c.aiBuilder != nil,
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
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowNew: no scope in context")
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
		c.deps.Logger.Error("console: workflow new view", "err", err)
		c.serveError(w, r)
		return
	}
	view := workflowsPageView{
		Header:       header,
		Fragment:     frag,
		Signals:      workflowsSignalsJSON(!c.deps.ReadOnly),
		ReadOnly:     c.deps.ReadOnly,
		FormError:    r.URL.Query().Get("error"),
		AIConfigured: c.aiBuilder != nil,
	}
	c.renderPage(w, r, http.StatusOK, "workflows", "Workflows", view)
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
		Signals:          workflowsSignalsJSON(false),
		ReadOnly:         c.deps.ReadOnly,
		AIConfigured:     c.aiBuilder != nil,
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
	q := mergeFragmentParams(r, "wfFilter", "wfStatus", "wfConfirmDelete")
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
			Label:   "Create workflow",
			OnClick: "$workflowNewOpen = true",
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
		Signals:           workflowsSignalsJSON(false),
		StepCatalog:       c.stepTypes.Catalog(),
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

// ─── W2-A5: test / trigger page ───────────────────────────────────────────────

// wfTestSignalsJSON returns the initial signals for the test page. The payload
// textarea is plain state managed by the browser; only the CSRF comes from the
// server.
func wfTestSignalsJSON() template.JS {
	b, _ := json.Marshal(map[string]string{"testSubscriberId": "", "testPayload": "{}"})
	return template.JS(b)
}

// serveWorkflowTestPage renders GET /console/workflows/{slug}/test (W2-A5).
func (c *Console) serveWorkflowTestPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWorkflowTestPage: w is nil")
	}
	if r == nil {
		panic("serveWorkflowTestPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveWorkflowTestPage: no scope in context")
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
		c.deps.Logger.Error("console: test page get workflow", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	steps, _ := decodeSteps(wf.Steps)

	payloadExample := "{}"
	if ex := payloadExampleFromSchema(wf.PayloadSchema); ex != "" {
		payloadExample = ex
	}

	s, _ := sessionFromContext(r.Context())
	header := pageHeader{
		Title:    wf.Name,
		Subtitle: fmt.Sprintf("Trigger identifier: %s", wf.TriggerIdentifier),
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: wf.Name}
	}

	view := workflowTestView{
		Header:            header,
		TriggerIdentifier: wf.TriggerIdentifier,
		Name:              wf.Name,
		Active:            wf.Active,
		StepCount:         len(steps),
		PayloadExample:    payloadExample,
		FormError:         r.URL.Query().Get("error"),
		TransactionID:     r.URL.Query().Get("txid"),
		InactiveBlocked:   !wf.Active,
		ReadOnly:          c.deps.ReadOnly,
		Signals:           wfTestSignalsJSON(),
		CSRF:              s.CSRF,
	}
	// If we have a txid, resolve the run id + deep-link from the notification
	// store so the operator sees the durable run reference (W4 ADR-013, #55).
	if txid := r.URL.Query().Get("txid"); txid != "" {
		view.RunID = c.runIDFromTransaction(r.Context(), scope, txid)
		view.RunURL = c.runURL(view.RunID)
	}

	c.renderPage(w, r, http.StatusOK, "workflow-test", wf.Name+" — Test", view)
}

// handleWorkflowTest handles POST /console/api/workflows/{slug}/test (W2-A5).
// Validates JSON payload, calls trigger.Ingest in-process, redirects to the
// test page with ?txid= on success or ?error= on failure.
func (c *Console) handleWorkflowTest(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowTest: w is nil")
	}
	if r == nil {
		panic("handleWorkflowTest: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowTest: no scope in context")
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

	testURL := "/console/workflows/" + slug + "/test"

	// Validate the workflow is active before calling Ingest.
	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: test trigger lookup", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	if !wf.Active {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Workflow is inactive. Activate it before sending a test trigger."),
			http.StatusSeeOther)
		return
	}

	subscriberID := strings.TrimSpace(r.FormValue("subscriber_id"))
	payloadRaw := strings.TrimSpace(r.FormValue("payload"))
	if subscriberID == "" {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Subscriber ID is required."), http.StatusSeeOther)
		return
	}
	if payloadRaw == "" {
		payloadRaw = "{}"
	}
	if !json.Valid([]byte(payloadRaw)) {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Payload must be valid JSON."), http.StatusSeeOther)
		return
	}

	if c.deps.Trigger == nil {
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Trigger service unavailable."), http.StatusSeeOther)
		return
	}

	req := trigger.TriggerRequest{
		Name: slug,
		To: trigger.ToField{
			SubscriberID: subscriberID,
		},
		Payload: json.RawMessage(payloadRaw),
	}
	resp, err := c.deps.Trigger.Ingest(r.Context(), scope, req)
	if err != nil {
		c.deps.Logger.Error("console: test trigger ingest", "slug", slug, "err", err)
		http.Redirect(w, r, testURL+"?error="+
			url.QueryEscape("Trigger failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	if !resp.Acknowledged {
		msg := "Trigger not acknowledged"
		if len(resp.Error) > 0 {
			msg = resp.Error[0]
		}
		http.Redirect(w, r, testURL+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	// Success: redirect to the test page with the transaction id surfaced.
	activityHref := "/console/workflows/" + slug + "/activity"
	_ = activityHref // surfaced in template via TransactionID link
	http.Redirect(w, r, testURL+"?txid="+url.QueryEscape(resp.TransactionID),
		http.StatusSeeOther)
}

// payloadExampleFromSchema derives a simple example JSON object from a
// payload schema. Returns "" when the schema is absent or unparseable.
// This is a best-effort hint for the test form's payload textarea.
func payloadExampleFromSchema(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	var s struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
			Example any    `json:"example"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return ""
	}
	out := make(map[string]any, len(s.Properties))
	for k, v := range s.Properties {
		switch {
		case v.Example != nil:
			out[k] = v.Example
		case v.Default != nil:
			out[k] = v.Default
		default:
			switch v.Type {
			case "string":
				out[k] = ""
			case "number", "integer":
				out[k] = 0
			case "boolean":
				out[k] = false
			default:
				out[k] = nil
			}
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
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
		row := c.buildActivityRow(n, msgs, wf.Name, c.runURL(n.RunID))
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

// ─── helpers ──────────────────────────────────────────────────────────────────

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

// conditionsFromFilters decodes step.Filters ([]filter.Group) back into the
// editor's combinator + per-row form model. Only the first Group is surfaced
// (the UI authors a single AND/OR group; no nested groups). The returned rows
// are padded to stepConditionRows so the editor renders a fixed pool of rows
// (the trailing blanks are hidden client-side until "Add condition" reveals
// them). visible is the count of saved conditions, clamped to at least 1, and
// seeds the $condRowCount signal. Combinator defaults to "AND" when absent.
func conditionsFromFilters(raw json.RawMessage) (combinator string, rows []stepConditionRow, visible int) {
	combinator = "AND"
	rows = make([]stepConditionRow, 0, stepConditionRows)
	if len(raw) > 0 {
		var groups []filter.Group
		if err := json.Unmarshal(raw, &groups); err == nil && len(groups) > 0 {
			g := groups[0]
			if g.Combinator != "" {
				combinator = strings.ToUpper(g.Combinator)
			}
			for _, c := range g.Conditions {
				rows = append(rows, stepConditionRow{
					On:       c.On,
					Field:    c.Field,
					Operator: string(c.Operator),
					Value:    conditionValueString(c.Value),
				})
			}
		}
	}
	visible = len(rows)
	if visible < 1 {
		visible = 1
	}
	if visible > stepConditionRows {
		visible = stepConditionRows
	}
	for len(rows) < stepConditionRows {
		rows = append(rows, stepConditionRow{})
	}
	return combinator, rows, visible
}

// listValueOperators are the operators whose form Value is a single text field
// holding a comma-separated list (IN/NOT_IN/ALL_IN/ANY_IN) or a "min,max" pair
// (BETWEEN/NOT_BETWEEN). conditionsFromForm parses them into a []any; the
// parse-back renders the []any as comma-joined text. Single-value operators
// (LIKE/NOT_LIKE, EQUAL, ...) keep a plain string Value.
var listValueOperators = map[filter.Operator]bool{
	filter.OpIn:         true,
	filter.OpNotIn:      true,
	filter.OpAllIn:      true,
	filter.OpAnyIn:      true,
	filter.OpBetween:    true,
	filter.OpNotBetween: true,
}

// conditionValueForOperator shapes the single Value text field into the
// evaluator's expected wire shape for the given operator. For the list/range
// operators it splits on "," into a []any (each piece trimmed); BETWEEN/
// NOT_BETWEEN coerce each bound to a number when possible so the wire value is
// a numeric [min,max] (matching the reference platform's between [min,max] number array), while
// IN-family keep string elements. All other operators keep the raw string.
func conditionValueForOperator(op filter.Operator, raw string) any {
	if !listValueOperators[op] {
		return raw
	}
	parts := strings.Split(raw, ",")
	out := make([]any, 0, len(parts))
	numeric := op == filter.OpBetween || op == filter.OpNotBetween
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if numeric {
			if f, err := strconv.ParseFloat(p, 64); err == nil {
				out = append(out, f)
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// conditionValueString renders a decoded condition Value back to the text the
// operator typed. Scalars round-trip; a []any (the IN-family list and the
// BETWEEN range) renders as comma-joined text so the single Value input
// repopulates. Other non-scalars come back empty.
func conditionValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = conditionValueString(e)
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// wireCondition / wireGroup mirror filter.Condition / filter.Group with the
// shared-contract json tags. The vendored filter types carry no struct tags
// (they rely on Go's case-insensitive unmarshal at eval time), so marshaling
// them directly would emit Go field-name keys ("On", "Combinator", ...) rather
// than the contract's lowercase keys ("on", "combinator", ...). We build
// filter.* values for type-safety, then convert to these tagged mirrors for the
// on-the-wire bytes the evaluator (and any other consumer) reads back.
type wireCondition struct {
	On       string          `json:"on"`
	Field    string          `json:"field"`
	Operator filter.Operator `json:"operator"`
	Value    any             `json:"value"`
	Negated  bool            `json:"negated"`
}

type wireGroup struct {
	Combinator string          `json:"combinator"`
	Conditions []wireCondition `json:"conditions"`
	Groups     []wireGroup     `json:"groups,omitempty"`
}

func toWireGroup(g filter.Group) wireGroup {
	wg := wireGroup{Combinator: g.Combinator}
	for _, c := range g.Conditions {
		wg.Conditions = append(wg.Conditions, wireCondition{
			On:       c.On,
			Field:    c.Field,
			Operator: c.Operator,
			Value:    c.Value,
			Negated:  c.Negated,
		})
	}
	for _, child := range g.Groups {
		wg.Groups = append(wg.Groups, toWireGroup(child))
	}
	return wg
}

// conditionsFromForm parses the posted CONDITIONS rows into a single
// filter.Group wrapped in a JSON array of Group, marshaled with the shared
// contract's lowercase keys. Rows with an empty Field are skipped. When no
// usable row exists it returns (nil, nil) so the step's Filters stay absent and
// the step is not gated.
func conditionsFromForm(r *http.Request) (json.RawMessage, error) {
	field := func(name string) string { return strings.TrimSpace(r.FormValue(name)) }

	combinator := strings.ToUpper(field("cond_combinator"))
	if combinator != "OR" {
		combinator = "AND"
	}

	var conditions []filter.Condition
	for i := 0; i < stepConditionRows; i++ {
		suffix := strconv.Itoa(i)
		path := field("cond_field_" + suffix)
		if path == "" {
			continue
		}
		on := field("cond_on_" + suffix)
		if on == "" {
			on = "payload"
		}
		op := filter.Operator(field("cond_operator_" + suffix))
		conditions = append(conditions, filter.Condition{
			On:       on,
			Field:    path,
			Operator: op,
			Value:    conditionValueForOperator(op, field("cond_value_"+suffix)),
		})
	}
	if len(conditions) == 0 {
		return nil, nil
	}
	groups := []wireGroup{toWireGroup(filter.Group{Combinator: combinator, Conditions: conditions})}
	b, err := json.Marshal(groups)
	if err != nil {
		return nil, fmt.Errorf("marshal conditions: %w", err)
	}
	return b, nil
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

// ─── W4-W3: Handlebars live-preview pane ─────────────────────────────────────

// serveFragmentStepPreview handles
// GET /console/api/fragments/step-preview.
//
// Query parameters (sent as Datastar signals via @get):
//
//	tmpl    — the Handlebars template string to render
//	payload — a JSON object whose keys become the {{payload.*}} namespace
//
// The endpoint is read-only (GET, no CSRF required, no state mutation).
// Template errors are returned as inline HTML — never a 500.
// Scope safety: raymond evaluates a logicless Handlebars template; there
// is no file I/O, no network access, and no helper registration — the
// sandbox is raymond's own strict evaluation model.
func (c *Console) serveFragmentStepPreview(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentStepPreview: w is nil")
	}
	if r == nil {
		panic("serveFragmentStepPreview: r is nil")
	}

	// Read signals (Datastar @get sends signals as a URL-encoded JSON query
	// param). Fall back to raw query params so plain HTTP GET also works
	// (browser navigation / tests).
	var tmplStr, payloadStr string
	env, ok := readSignalsBestEffort(r)
	if ok {
		tmplStr = env.WfPreviewTmpl
		payloadStr = env.WfPreviewPayload
	}
	// URL query params override / supplement signal values so a plain
	// GET ?tmpl=…&payload=… (curl, tests) also works.
	if q := r.URL.Query().Get("tmpl"); q != "" {
		tmplStr = q
	}
	if q := r.URL.Query().Get("payload"); q != "" {
		payloadStr = q
	}

	view := renderStepPreview(tmplStr, payloadStr)

	html, err := renderFragment(c.ts.base, "step-preview", view)
	if err != nil {
		c.deps.Logger.Error("console: render step-preview fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// renderStepPreview renders tmplStr against a data map built from
// payloadStr (a JSON object). It is extracted from the handler so tests
// can call it without HTTP plumbing.
//
// The data namespace is byte-for-byte the send-time context built in
// internal/delivery/delivery.go (and email.go / push.go):
//
//	data := map[string]any{"subscriber": subMap, "payload": payloadMap}
//
// so a body that renders {{payload.name}} in the preview renders the same
// at send time. A passthrough {{t}} helper is registered to mirror
// template.RenderI18n — an unseeded translate key renders verbatim, so a
// template that uses {{t "key"}} previews without a spurious error.
// There is no subscriber at design time, so {{subscriber.*}} resolves to
// "" (the same fail-soft behaviour the engine uses for an absent path).
//
// Template errors (parse or execute) and bad payload JSON surface as
// stepPreviewView.Error rather than panicking or returning a 500. This
// uses raymond directly (the same library internal/delivery/template
// wraps) so we can distinguish a compile error from a successful render
// — the package-level Render helper deliberately swallows errors into the
// literal source, which a preview pane must not do.
func renderStepPreview(tmplStr, payloadStr string) stepPreviewView {
	// Parse the template first so we can report compile errors distinctly
	// from execute errors. raymond.Parse returns an error on malformed
	// Handlebars (unterminated {{ etc.).
	tpl, err := raymond.Parse(tmplStr)
	if err != nil {
		return stepPreviewView{IsError: true, Error: "Template error: " + err.Error()}
	}
	// Mirror template.RenderI18n: an unseeded key renders verbatim.
	tpl.RegisterHelper("t", func(key string) string { return key })

	// Build the data context. payload is a JSON object; if it is empty or
	// not valid JSON we report the JSON error so the operator can fix it.
	payloadData := make(map[string]any)
	if payloadStr != "" {
		if jsonErr := json.Unmarshal([]byte(payloadStr), &payloadData); jsonErr != nil {
			return stepPreviewView{IsError: true, Error: "Payload JSON error: " + jsonErr.Error()}
		}
	}

	data := map[string]any{
		"payload":    payloadData,
		"subscriber": map[string]any{},
	}

	out, err := tpl.Exec(data)
	if err != nil {
		return stepPreviewView{IsError: true, Error: "Render error: " + err.Error()}
	}
	return stepPreviewView{Output: out}
}
