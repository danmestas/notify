package console

// layouts_console.go implements W4-L1 (layout list + create + editor) for the
// operator console.
//
// All data access is in-process via Deps.Layouts (store.LayoutRepo). No
// loopback HTTP hops. Every mutation handler is gated on Deps.ReadOnly
// server-side; mutations answer with a 303 redirect (never SSE to a native
// form POST).

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ─── signal keys ─────────────────────────────────────────────────────────────

var layoutSignalKeys = []string{
	"layoutsFilter",
}

// layoutsPageLimit caps a single layouts-list fragment page.
const layoutsPageLimit = 50

// ─── view structs ─────────────────────────────────────────────────────────────

// layoutsPageView is what layouts.html binds.
type layoutsPageView struct {
	Header   pageHeader
	Fragment layoutsListView
	Signals  template.JS
	ReadOnly bool
	// ShowCreate renders the create-layout slide-over (/console/layouts/new).
	ShowCreate bool
}

// layoutsListView is the #layouts-tbody fragment binding.
type layoutsListView struct {
	Rows     []layoutRow
	NextKey  string
	ReadOnly bool
	FilterQ  string
	// ConfirmDelete names the layout key awaiting inline two-step delete
	// confirmation (chassis idiom — no native confirm() dialogs).
	ConfirmDelete string
	// CSRF is the session token for the POST forms rendered inside the fragment.
	CSRF string
}

// layoutRow is one <tr> in the layouts table.
type layoutRow struct {
	Key  string
	Name string
}

// layoutEditorView is what layout_editor.html binds.
type layoutEditorView struct {
	Header   pageHeader
	Layout   domain.Layout
	Signals  template.JS
	ReadOnly bool
	// ConfirmDelete renders the inline two-step delete confirmation
	// (?confirmDelete=true).
	ConfirmDelete bool
	CSRF          string
}

// layoutsSignalsJSON renders the initial Datastar signal envelope for the
// layouts page. The filter signal is plumbed now so the fragment envelope
// vocabulary knows it exists (future filter UI).
func layoutsSignalsJSON(filter string) template.JS {
	sig := map[string]string{
		"layoutsFilter": filter,
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view builder ─────────────────────────────────────────────────────────────

// buildLayoutsListView reads the optional confirmDelete + after URL params,
// lists the scope's layouts, and assembles the fragment binding.
func (c *Console) buildLayoutsListView(r *http.Request) (layoutsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildLayoutsListView: no scope in context")
	}

	params := mergeFragmentParams(r, layoutSignalKeys...)
	filter := ""
	if vs := params["q"]; len(vs) > 0 {
		filter = strings.ToLower(strings.TrimSpace(vs[0]))
	}

	after := r.URL.Query().Get("after")
	layouts, nextKey, err := c.deps.Layouts.List(r.Context(), scope, after, layoutsPageLimit)
	if err != nil {
		return layoutsListView{}, err
	}

	rows := make([]layoutRow, 0, len(layouts))
	for _, l := range layouts {
		if filter != "" &&
			!strings.Contains(strings.ToLower(l.Key), filter) &&
			!strings.Contains(strings.ToLower(l.Name), filter) {
			continue
		}
		rows = append(rows, layoutRow{Key: l.Key, Name: l.Name})
	}

	s, _ := sessionFromContext(r.Context())
	return layoutsListView{
		Rows:          rows,
		NextKey:       nextKey,
		ReadOnly:      c.deps.ReadOnly,
		FilterQ:       filter,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		CSRF:          s.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveLayoutsPage renders GET /console/layouts.
func (c *Console) serveLayoutsPage(w http.ResponseWriter, r *http.Request) {
	c.renderLayoutsPage(w, r, false)
}

// serveLayoutsNew renders GET /console/layouts/new with the create slide-over
// visible.
func (c *Console) serveLayoutsNew(w http.ResponseWriter, r *http.Request) {
	c.renderLayoutsPage(w, r, true)
}

// renderLayoutsPage is the shared body of the list and create-slide-over
// pages; showCreate toggles the slide-over.
func (c *Console) renderLayoutsPage(w http.ResponseWriter, r *http.Request, showCreate bool) {
	if w == nil {
		panic("renderLayoutsPage: w is nil")
	}
	if r == nil {
		panic("renderLayoutsPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("renderLayoutsPage: no scope in context")
	}

	frag, err := c.buildLayoutsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: layouts page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Layouts",
		Subtitle: "Reusable content wrappers that surround notification bodies.",
		Tiles: []tile{
			{Label: "layouts", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Layouts"}
	}

	title := "Layouts"
	if showCreate {
		title = "Create layout"
	}
	view := layoutsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  layoutsSignalsJSON(""),
		ReadOnly: c.deps.ReadOnly,
		// The create slide-over is the whole point of /new; read-only
		// consoles render the plain list instead (mutations are gated
		// server-side regardless).
		ShowCreate: showCreate && !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "layouts", title, view)
}

// serveFragmentLayoutsList handles GET /console/api/fragments/layouts-list.
func (c *Console) serveFragmentLayoutsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentLayoutsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentLayoutsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentLayoutsList: no scope in context")
	}

	view, err := c.buildLayoutsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: layouts fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "layouts-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render layouts fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// serveLayoutEditor renders GET /console/layouts/{key}.
func (c *Console) serveLayoutEditor(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveLayoutEditor: w is nil")
	}
	if r == nil {
		panic("serveLayoutEditor: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveLayoutEditor: no scope in context")
	}

	key := chi.URLParam(r, "key")
	l, err := c.deps.Layouts.Get(r.Context(), scope, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: layout editor get", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Edit layout",
		Subtitle: l.Key,
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Edit layout"}
	}

	s, _ := sessionFromContext(r.Context())
	view := layoutEditorView{
		Header:        header,
		Layout:        l,
		Signals:       layoutsSignalsJSON(""),
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		CSRF:          s.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "layout-editor", "Edit layout", view)
}

// serveFragmentLayoutEditor handles GET /console/api/fragments/layout-editor?key=X.
func (c *Console) serveFragmentLayoutEditor(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentLayoutEditor: w is nil")
	}
	if r == nil {
		panic("serveFragmentLayoutEditor: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveFragmentLayoutEditor: no scope in context")
	}

	key := r.URL.Query().Get("key")
	l, err := c.deps.Layouts.Get(r.Context(), scope, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "layout not found", http.StatusNotFound)
			return
		}
		c.deps.Logger.Error("console: layout editor fragment get", "err", err)
		http.Error(w, "get failed", http.StatusInternalServerError)
		return
	}

	s, _ := sessionFromContext(r.Context())
	view := layoutEditorView{
		Layout:        l,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		CSRF:          s.CSRF,
	}
	// The editor markup lives in the layout-editor page clone's `content`
	// template (the data-signals wrapper carries the patch target id).
	tmpl, ok := c.ts.pageTemplates["layout-editor"]
	if !ok {
		panic("serveFragmentLayoutEditor: layout-editor template not registered")
	}
	html, err := renderFragment(tmpl, "content", pageData{
		Section:  "layout-editor",
		CSRF:     s.CSRF,
		ReadOnly: c.deps.ReadOnly,
		Page:     view,
	})
	if err != nil {
		c.deps.Logger.Error("console: render layout editor fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleLayoutCreate handles POST /console/api/layouts.
func (c *Console) handleLayoutCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleLayoutCreate: w is nil")
	}
	if r == nil {
		panic("handleLayoutCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleLayoutCreate: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	name := strings.TrimSpace(r.FormValue("name"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if _, err := c.deps.Layouts.Create(r.Context(), scope, key, name, ""); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Re-render the create slide-over with an inline conflict notice.
			c.renderLayoutsCreateConflict(w, r, key, name)
			return
		}
		c.deps.Logger.Error("console: create layout", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/layouts/"+key, http.StatusSeeOther)
}

// renderLayoutsCreateConflict re-renders the layouts page with the create
// slide-over open and a conflict notice on the duplicate key.
func (c *Console) renderLayoutsCreateConflict(w http.ResponseWriter, r *http.Request, key, name string) {
	frag, err := c.buildLayoutsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: layouts conflict list", "err", err)
		c.serveError(w, r)
		return
	}
	header := pageHeader{
		Title:    "Layouts",
		Subtitle: "A layout with key " + key + " already exists.",
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Layouts"}
	}
	view := layoutsPageView{
		Header:     header,
		Fragment:   frag,
		Signals:    layoutsSignalsJSON(""),
		ReadOnly:   c.deps.ReadOnly,
		ShowCreate: !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusConflict, "layouts", "Create layout", view)
}

// handleLayoutSave handles POST /console/api/layouts/{key}/save. Save is an
// upsert; it writes name+content for the key, then 303-redirects back to the
// editor.
func (c *Console) handleLayoutSave(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleLayoutSave: w is nil")
	}
	if r == nil {
		panic("handleLayoutSave: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleLayoutSave: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := chi.URLParam(r, "key")
	name := strings.TrimSpace(r.FormValue("name"))
	content := r.FormValue("content")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if err := c.deps.Layouts.Upsert(r.Context(), scope, key, name, content); err != nil {
		c.deps.Logger.Error("console: save layout", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/layouts/"+key, http.StatusSeeOther)
}

// handleLayoutDelete handles POST /console/api/layouts/{key}/delete.
func (c *Console) handleLayoutDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleLayoutDelete: w is nil")
	}
	if r == nil {
		panic("handleLayoutDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleLayoutDelete: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	key := chi.URLParam(r, "key")
	if err := c.deps.Layouts.Delete(r.Context(), scope, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete layout", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/layouts", http.StatusSeeOther)
}
