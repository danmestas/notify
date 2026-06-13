package console

// variables.go implements W4-V2 (variables list + create sheet + detail/editor)
// for the operator console.
//
// All data access is in-process via Deps.EnvVars (*envvars.Service). No
// loopback HTTP hops. Every mutation handler is gated on Deps.ReadOnly
// server-side. Values are always masked in the list view; the detail page
// has a ?reveal=1 query param.

import (
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/store"
)

// ─── signal keys ─────────────────────────────────────────────────────────────

var variableSignalKeys = []string{
	"variablesSearch",
}

// ─── view structs ─────────────────────────────────────────────────────────────

type variablesPageView struct {
	Header     pageHeader
	Fragment   variablesListView
	Signals    template.JS
	ReadOnly   bool
	ShowCreate bool
}

type variablesListView struct {
	Rows          []variableRow
	ReadOnly      bool
	ConfirmDelete string
	CSRF          string
}

type variableRow struct {
	ID          string
	Key         string
	IsSecret    bool
	MaskedValue string
	UpdatedAt   string
}

type variableDetailView struct {
	Header        pageHeader
	EV            domain.EnvVar
	Value         string
	Revealed      bool
	ReadOnly      bool
	ConfirmDelete bool
	FormError     string
	CSRF          string
	Signals       template.JS
}

func variablesSignalsJSON(search string) template.JS {
	// Use json.Marshal manually so we don't import encoding/json at the top level —
	// the value is simple strings so manual construction is safe.
	s := `{"variablesSearch":` + `"` + strings.ReplaceAll(search, `"`, `\"`) + `"}`
	return template.JS(s)
}

// ─── view builder ─────────────────────────────────────────────────────────────

func (c *Console) buildVariablesListView(r *http.Request) (variablesListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildVariablesListView: no scope in context")
	}
	params := mergeFragmentParams(r, variableSignalKeys...)
	search := ""
	if v := params["variablesSearch"]; len(v) > 0 {
		search = v[0]
	}

	items, _, _, err := c.deps.EnvVars.List(r.Context(), scope, search, "", "", 50)
	if err != nil {
		return variablesListView{}, err
	}

	rows := make([]variableRow, 0, len(items))
	for _, ev := range items {
		maskedValue := envvars.ValueMaskSentinel
		if !ev.IsSecret {
			// Non-secrets: reveal inline since there's no confidentiality risk.
			plain, err := c.deps.EnvVars.GetValue(r.Context(), scope, ev.Key)
			if err == nil {
				maskedValue = plain
			}
			// On error (shouldn't happen), fall back to sentinel — fail-closed.
		}
		rows = append(rows, variableRow{
			ID:          ev.ID,
			Key:         ev.Key,
			IsSecret:    ev.IsSecret,
			MaskedValue: maskedValue,
			UpdatedAt:   ev.UpdatedAt,
		})
	}

	s, _ := sessionFromContext(r.Context())
	return variablesListView{
		Rows:          rows,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		CSRF:          s.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

func (c *Console) serveVariablesPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveVariablesPage: w is nil")
	}
	if r == nil {
		panic("serveVariablesPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveVariablesPage: no scope in context")
	}

	frag, err := c.buildVariablesListView(r)
	if err != nil {
		c.deps.Logger.Error("console: variables page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Variables",
		Subtitle: "Per-environment key/value pairs stored encrypted at rest.",
		Tiles: []tile{
			{Label: "variables", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Variables"}
	}

	view := variablesPageView{
		Header:   header,
		Fragment: frag,
		Signals:  variablesSignalsJSON(""),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "variables", "Variables", view)
}

func (c *Console) serveVariableNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveVariableNew: w is nil")
	}
	if r == nil {
		panic("serveVariableNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveVariableNew: no scope in context")
	}

	frag, err := c.buildVariablesListView(r)
	if err != nil {
		c.deps.Logger.Error("console: variables new list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Variables",
		Subtitle: "Per-environment key/value pairs stored encrypted at rest.",
		Tiles: []tile{
			{Label: "variables", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Variables"}
	}

	view := variablesPageView{
		Header:     header,
		Fragment:   frag,
		Signals:    variablesSignalsJSON(""),
		ReadOnly:   c.deps.ReadOnly,
		ShowCreate: !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "variables", "New variable", view)
}

func (c *Console) serveVariableDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveVariableDetail: w is nil")
	}
	if r == nil {
		panic("serveVariableDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveVariableDetail: no scope in context")
	}

	key := chi.URLParam(r, "key")
	reveal := r.URL.Query().Get("reveal") == "1"

	ev, err := c.deps.EnvVars.Get(r.Context(), scope, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: variable detail", "err", err)
		c.serveError(w, r)
		return
	}

	value := envvars.ValueMaskSentinel
	if reveal {
		plain, err := c.deps.EnvVars.GetValue(r.Context(), scope, key)
		if err != nil {
			// Fail-closed: decrypt error → 500.
			c.deps.Logger.Error("console: variable reveal", "err", err)
			c.serveError(w, r)
			return
		}
		value = plain
	}

	s, _ := sessionFromContext(r.Context())
	view := variableDetailView{
		Header: pageHeader{
			Title:    key,
			Subtitle: "Environment variable detail.",
		},
		EV:            ev,
		Value:         value,
		Revealed:      reveal,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		CSRF:          s.CSRF,
		Signals:       variablesSignalsJSON(""),
	}
	c.renderPage(w, r, http.StatusOK, "variable-detail", key, view)
}

func (c *Console) serveFragmentVariablesList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentVariablesList: w is nil")
	}
	if r == nil {
		panic("serveFragmentVariablesList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentVariablesList: no scope in context")
	}

	view, err := c.buildVariablesListView(r)
	if err != nil {
		c.deps.Logger.Error("console: variables fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "variables-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render variables fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

func (c *Console) handleVariableCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleVariableCreate: w is nil")
	}
	if r == nil {
		panic("handleVariableCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleVariableCreate: no scope in context")
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
	value := r.FormValue("value")
	isSecret := r.FormValue("is_secret") == "1" || r.FormValue("is_secret") == "on"

	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	if _, err := c.deps.EnvVars.Set(r.Context(), scope, key, isSecret, value); err != nil {
		c.deps.Logger.Error("console: create variable", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/variables/"+key, http.StatusSeeOther)
}

func (c *Console) handleVariableUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleVariableUpdate: w is nil")
	}
	if r == nil {
		panic("handleVariableUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleVariableUpdate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	key := chi.URLParam(r, "key")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	isSecret := r.FormValue("is_secret") == "1" || r.FormValue("is_secret") == "on"
	value := r.FormValue("value")

	// If value is empty or the sentinel, keep existing value.
	if value == "" || value == envvars.ValueMaskSentinel {
		plain, err := c.deps.EnvVars.GetValue(r.Context(), scope, key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				c.serveNotFound(w, r)
				return
			}
			c.deps.Logger.Error("console: variable update read existing", "err", err)
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		value = plain
	}

	if _, err := c.deps.EnvVars.Set(r.Context(), scope, key, isSecret, value); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: update variable", "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/variables/"+key, http.StatusSeeOther)
}

func (c *Console) handleVariableDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleVariableDelete: w is nil")
	}
	if r == nil {
		panic("handleVariableDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleVariableDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	key := chi.URLParam(r, "key")

	if err := c.deps.EnvVars.Delete(r.Context(), scope, key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete variable", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/variables", http.StatusSeeOther)
}
