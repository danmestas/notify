package console

// environments.go implements W4-E1/E2 — the environments management pages
// under /console/settings/environments.
//
// All data access is in-process via Deps.Identity (*identity.Service) for
// environment creation and Deps.Store.Environments() for List/Update/Delete.
// No loopback HTTP hops. Every mutation is gated on Deps.ReadOnly.
//
// Idioms:
//   - POST → 303 (never SSE to a native form POST)
//   - Two-step inline confirm for deletes (no native confirm())
//   - Last-environment guard: refuse deletion when CountByOrg ≤ 1
//   - Current-environment guard: refuse deletion of the scope's own env

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
)

// ─── view structs ─────────────────────────────────────────────────────────────

type environmentsPageView struct {
	Header   pageHeader
	Fragment environmentsListView
	Signals  template.JS
	ReadOnly bool
}

type environmentsListView struct {
	Rows            []environmentRow
	CurrentEnvID    string
	ReadOnly        bool
	ConfirmDelete   string // env ID awaiting two-step confirm
	LastEnvDeleteID string // set when user tried to delete the last env
	CSRF            string
}

type environmentRow struct {
	ID         string
	Name       string
	Identifier string
	Type       string
	IsCurrent  bool
}

func environmentsSignalsJSON(filter string, newOpen bool) template.JS {
	sig := map[string]any{
		"environmentsFilter": filter,
		// envNewOpen drives the create slide-over's data-show. A real JSON
		// boolean is required — a string "false" is truthy in JS.
		"envNewOpen": newOpen,
		// envEditOpen drives the edit/rename slide-over's data-show.
		"envEditOpen": false,
		// envEditID / envEditName are set by the Edit button click so the
		// slide-over form can embed the target id and pre-fill the name input.
		"envEditID":   "",
		"envEditName": "",
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view builder ─────────────────────────────────────────────────────────────

func (c *Console) buildEnvironmentsListView(r *http.Request) (environmentsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildEnvironmentsListView: no scope in context")
	}
	s, _ := sessionFromContext(r.Context())

	envs, err := c.deps.Identity.Store().Environments().List(r.Context(), scope.OrgID)
	if err != nil {
		return environmentsListView{}, err
	}

	rows := make([]environmentRow, 0, len(envs))
	for _, env := range envs {
		rows = append(rows, environmentRow{
			ID:         env.ID,
			Name:       env.Name,
			Identifier: env.Identifier,
			Type:       env.Type,
			IsCurrent:  env.ID == scope.EnvID,
		})
	}

	return environmentsListView{
		Rows:          rows,
		CurrentEnvID:  scope.EnvID,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		CSRF:          s.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

func (c *Console) serveEnvironmentsPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveEnvironmentsPage: w is nil")
	}
	if r == nil {
		panic("serveEnvironmentsPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveEnvironmentsPage: no scope in context")
	}

	frag, err := c.buildEnvironmentsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: environments page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Environments",
		Subtitle: "Isolated tenancy spaces. Each has its own API key and configuration.",
		Tiles: []tile{
			{Label: "environments", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Environments"}
	}

	view := environmentsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  environmentsSignalsJSON("", false),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "environments", "Environments", view)
}

func (c *Console) serveEnvironmentNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveEnvironmentNew: w is nil")
	}
	if r == nil {
		panic("serveEnvironmentNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveEnvironmentNew: no scope in context")
	}

	frag, err := c.buildEnvironmentsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: environment new list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Environments",
		Subtitle: "Isolated tenancy spaces. Each has its own API key and configuration.",
		Tiles: []tile{
			{Label: "environments", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Environments"}
	}

	view := environmentsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  environmentsSignalsJSON("", !c.deps.ReadOnly),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "environments", "Environments", view)
}

func (c *Console) serveFragmentEnvironmentsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentEnvironmentsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentEnvironmentsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentEnvironmentsList: no scope in context")
	}

	view, err := c.buildEnvironmentsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: environments fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "environments-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render environments fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleEnvironmentCreate handles POST /console/settings/environments.
func (c *Console) handleEnvironmentCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleEnvironmentCreate: w is nil")
	}
	if r == nil {
		panic("handleEnvironmentCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleEnvironmentCreate: no scope in context")
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
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	envType := strings.TrimSpace(r.FormValue("type"))

	if _, _, _, err := c.deps.Identity.CreateEnvironment(r.Context(), scope.OrgID, name, envType); err != nil {
		c.deps.Logger.Error("console: create environment", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/settings/environments", http.StatusSeeOther)
}

// handleEnvironmentUpdate handles POST /console/api/environments/{id}.
func (c *Console) handleEnvironmentUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleEnvironmentUpdate: w is nil")
	}
	if r == nil {
		panic("handleEnvironmentUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleEnvironmentUpdate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if err := c.deps.Identity.Store().Environments().Update(r.Context(), scope.OrgID, id, name); err != nil {
		c.deps.Logger.Error("console: update environment", "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/settings/environments", http.StatusSeeOther)
}

// handleEnvironmentDelete handles POST /console/api/environments/{id}/delete.
func (c *Console) handleEnvironmentDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleEnvironmentDelete: w is nil")
	}
	if r == nil {
		panic("handleEnvironmentDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleEnvironmentDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	// Guard: cannot delete the current session environment.
	if id == scope.EnvID {
		http.Redirect(w, r, "/console/settings/environments?error=current", http.StatusSeeOther)
		return
	}

	// Guard: cannot delete the last environment.
	count, err := c.deps.Identity.Store().Environments().CountByOrg(r.Context(), scope.OrgID)
	if err != nil {
		c.deps.Logger.Error("console: count environments for delete guard", "err", err)
		http.Error(w, "check failed", http.StatusInternalServerError)
		return
	}
	if count <= 1 {
		http.Redirect(w, r, "/console/settings/environments?error=last", http.StatusSeeOther)
		return
	}

	if err := c.deps.Identity.Store().Environments().Delete(r.Context(), scope.OrgID, id); err != nil {
		c.deps.Logger.Error("console: delete environment", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/settings/environments", http.StatusSeeOther)
}
