package console

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// workflows.go renders the workflows list and the create + duplicate
// side-sheets (W2-A3):
//
//   - GET  /console/workflows                            list page
//   - GET  /console/api/fragments/workflows-list         list tbody fragment
//   - GET  /console/workflows/new                        create slide-over
//   - GET  /console/workflows/{slug}/duplicate           duplicate slide-over
//   - POST /console/api/workflows                         create
//   - POST /console/api/workflows/{slug}/duplicate        duplicate
//
// The full workflow editor lives in workflow_builder.go (detail page + step
// CRUD), workflow_step_conditions.go (CONDITIONS row authoring + live
// preview), and workflow_fire_test_page.go (fire-test page). Version history,
// activate/rollback/delete, and the activity tab live in
// workflow_versions.go.
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
	// FormName and FormTriggerIdentifier repopulate the create form's Name
	// and Trigger id inputs after a validation-error redirect (?name=,
	// ?trigger_identifier=) so the operator does not have to retype a value
	// that was already correct just because a sibling field failed (M7).
	FormName              string
	FormTriggerIdentifier string
	// AIConfigured reports whether the AI workflow builder is wired (an
	// ANTHROPIC_API_KEY is set). When false the Guided create tab shows a
	// not-configured note instead of the prompt form.
	AIConfigured bool
	// AIBuilderSuggestions are clickable example-prompt cards shown in the
	// Guided create tab to help users get started. Each string is a complete
	// example prompt; clicking a card prefills the prompt textarea.
	AIBuilderSuggestions []string
	// Source workflow info for the duplicate slide-over pre-fill.
	SourceName       string
	SourceSlug       string
	SuggestedName    string
	SuggestedTrigger string
}

// workflowsSignalsJSON returns the editor/list page's Datastar signals.
// newOpen seeds workflowNewOpen, which drives the create slide-over's
// data-show. It MUST be a real JSON boolean (map[string]any) — the string
// "false" is truthy in JS and would leave the slide-over open.
func workflowsSignalsJSON(newOpen bool) template.JS {
	b, err := json.Marshal(map[string]any{
		"wfTab":           "editor",
		"workflowNewOpen": newOpen,
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
		Header:               header,
		Fragment:             frag,
		Signals:              workflowsSignalsJSON(false),
		ReadOnly:             c.deps.ReadOnly,
		AIConfigured:         c.aiBuilder != nil,
		AIBuilderSuggestions: aiBuilderCreateSuggestions,
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
		Header:                header,
		Fragment:              frag,
		Signals:               workflowsSignalsJSON(!c.deps.ReadOnly),
		ReadOnly:              c.deps.ReadOnly,
		FormError:             r.URL.Query().Get("error"),
		FormName:              r.URL.Query().Get("name"),
		FormTriggerIdentifier: r.URL.Query().Get("trigger_identifier"),
		AIConfigured:          c.aiBuilder != nil,
		AIBuilderSuggestions:  aiBuilderCreateSuggestions,
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

	// redirectWithError sends the operator back to the create form with an
	// inline error, preserving the submitted Name + Trigger id (M7) so a
	// failure on one field doesn't force retyping the other.
	redirectWithError := func(msg string) {
		http.Redirect(w, r, "/console/workflows/new?error="+url.QueryEscape(msg)+
			"&name="+url.QueryEscape(name)+"&trigger_identifier="+url.QueryEscape(trigger),
			http.StatusSeeOther)
	}

	if name == "" || trigger == "" {
		redirectWithError("Name and trigger identifier are required.")
		return
	}

	// Reject a duplicate identifier before writing — surface it inline.
	if _, err := c.deps.Authoring.GetBySlug(r.Context(), scope, trigger); err == nil {
		redirectWithError("Trigger identifier already exists.")
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
		if errors.Is(err, authoring.ErrInvalidTriggerIdentifier) {
			redirectWithError("Trigger identifier may only contain lowercase letters, numbers, hyphens, and underscores.")
			return
		}
		c.deps.Logger.Error("console: create workflow", "err", err)
		redirectWithError("Create failed.")
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

// isNotFound reports whether err is a store not-found sentinel.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
