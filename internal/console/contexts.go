package console

// contexts.go implements W4-C2 (contexts list + create sheet + detail/editor)
// for the operator console.
//
// All data access is in-process via Deps.Contexts (*contexts.Service). No
// loopback HTTP hops. Every mutation handler is gated on Deps.ReadOnly
// server-side.

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

var contextSignalKeys = []string{
	"contextsType",
	"contextsSearch",
}

// ─── view structs ─────────────────────────────────────────────────────────────

type contextsPageView struct {
	Header     pageHeader
	Fragment   contextsListView
	Signals    template.JS
	ReadOnly   bool
	ShowCreate bool
}

type contextsListView struct {
	Rows          []contextRow
	ReadOnly      bool
	ConfirmDelete string
	FilterType    string
	CSRF          string
}

type contextRow struct {
	ID         string
	Type       string
	ExternalID string
	UpdatedAt  string
}

type contextDetailView struct {
	Header        pageHeader
	Ctx           domain.Context
	DataPretty    string
	ReadOnly      bool
	ConfirmDelete bool
	FormError     string
	CSRF          string
	Signals       template.JS
}

func contextsSignalsJSON(ctype string) template.JS {
	m := map[string]string{
		"contextsType":   ctype,
		"contextsSearch": "",
	}
	b, err := json.Marshal(m)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view builder ─────────────────────────────────────────────────────────────

func (c *Console) buildContextsListView(r *http.Request) (contextsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildContextsListView: no scope in context")
	}
	params := mergeFragmentParams(r, contextSignalKeys...)
	filterType := ""
	if v := params["type"]; len(v) > 0 {
		filterType = v[0]
	}
	search := ""
	if v := params["q"]; len(v) > 0 {
		search = v[0]
	}

	items, _, _, err := c.deps.Contexts.List(r.Context(), scope, filterType, search, "", "", 50)
	if err != nil {
		return contextsListView{}, err
	}

	rows := make([]contextRow, 0, len(items))
	for _, ctx := range items {
		rows = append(rows, contextRow{
			ID:         ctx.ID,
			Type:       ctx.Type,
			ExternalID: ctx.ExternalID,
			UpdatedAt:  ctx.UpdatedAt,
		})
	}

	s, _ := sessionFromContext(r.Context())
	return contextsListView{
		Rows:          rows,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		FilterType:    filterType,
		CSRF:          s.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

func (c *Console) serveContextsPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveContextsPage: w is nil")
	}
	if r == nil {
		panic("serveContextsPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveContextsPage: no scope in context")
	}

	frag, err := c.buildContextsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: contexts page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Contexts",
		Subtitle: "Reusable JSON data objects keyed by type and ID.",
		Tiles: []tile{
			{Label: "contexts", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Contexts"}
	}

	view := contextsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  contextsSignalsJSON(frag.FilterType),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "contexts", "Contexts", view)
}

func (c *Console) serveContextNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveContextNew: w is nil")
	}
	if r == nil {
		panic("serveContextNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveContextNew: no scope in context")
	}

	frag, err := c.buildContextsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: contexts new list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Contexts",
		Subtitle: "Reusable JSON data objects keyed by type and ID.",
		Tiles: []tile{
			{Label: "contexts", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Contexts"}
	}

	view := contextsPageView{
		Header:     header,
		Fragment:   frag,
		Signals:    contextsSignalsJSON(""),
		ReadOnly:   c.deps.ReadOnly,
		ShowCreate: !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "contexts", "New context", view)
}

func (c *Console) serveContextDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveContextDetail: w is nil")
	}
	if r == nil {
		panic("serveContextDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveContextDetail: no scope in context")
	}

	ctype := chi.URLParam(r, "type")
	externalID := chi.URLParam(r, "id")

	ctx, err := c.deps.Contexts.Get(r.Context(), scope, ctype, externalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: context detail", "err", err)
		c.serveError(w, r)
		return
	}

	pretty, _ := json.MarshalIndent(ctx.Data, "", "  ")

	s, _ := sessionFromContext(r.Context())
	view := contextDetailView{
		Header: pageHeader{
			Title:    ctx.Type + "/" + ctx.ExternalID,
			Subtitle: "Context data editor.",
		},
		Ctx:           ctx,
		DataPretty:    string(pretty),
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		CSRF:          s.CSRF,
		Signals:       contextsSignalsJSON(""),
	}
	c.renderPage(w, r, http.StatusOK, "context-detail", ctx.Type+"/"+ctx.ExternalID, view)
}

func (c *Console) serveFragmentContextsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentContextsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentContextsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentContextsList: no scope in context")
	}

	view, err := c.buildContextsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: contexts fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "contexts-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render contexts fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

func (c *Console) handleContextCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleContextCreate: w is nil")
	}
	if r == nil {
		panic("handleContextCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleContextCreate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	ctype := strings.TrimSpace(r.FormValue("type"))
	externalID := strings.TrimSpace(r.FormValue("id"))
	rawData := strings.TrimSpace(r.FormValue("data"))

	if ctype == "" || externalID == "" {
		http.Error(w, "type and id are required", http.StatusBadRequest)
		return
	}

	var data json.RawMessage
	if rawData == "" {
		data = json.RawMessage("{}")
	} else {
		if !json.Valid([]byte(rawData)) {
			// Surface JSON parse errors inline via redirect — stays pure HTML,
			// no JS, and never a 500.
			http.Redirect(w, r, "/console/contexts/new?dataError=invalid+JSON", http.StatusSeeOther)
			return
		}
		data = json.RawMessage(rawData)
	}

	created, err := c.deps.Contexts.Create(r.Context(), scope, ctype, externalID, data)
	if err != nil {
		if errors.Is(err, store.ErrContextConflict) {
			http.Redirect(w, r, "/console/contexts/new?conflict=1", http.StatusSeeOther)
			return
		}
		c.deps.Logger.Error("console: create context", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/contexts/"+created.Type+"/"+created.ExternalID, http.StatusSeeOther)
}

func (c *Console) handleContextUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleContextUpdate: w is nil")
	}
	if r == nil {
		panic("handleContextUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleContextUpdate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	ctype := chi.URLParam(r, "type")
	externalID := chi.URLParam(r, "id")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	rawData := strings.TrimSpace(r.FormValue("data"))
	var data json.RawMessage
	if rawData == "" {
		data = json.RawMessage("{}")
	} else {
		if !json.Valid([]byte(rawData)) {
			http.Redirect(w, r, "/console/contexts/"+ctype+"/"+externalID+"?dataError=invalid+JSON", http.StatusSeeOther)
			return
		}
		data = json.RawMessage(rawData)
	}

	if _, err := c.deps.Contexts.Update(r.Context(), scope, ctype, externalID, data); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: update context", "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/contexts/"+ctype+"/"+externalID, http.StatusSeeOther)
}

func (c *Console) handleContextDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleContextDelete: w is nil")
	}
	if r == nil {
		panic("handleContextDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleContextDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	ctype := chi.URLParam(r, "type")
	externalID := chi.URLParam(r, "id")

	if err := c.deps.Contexts.Delete(r.Context(), scope, ctype, externalID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete context", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/contexts", http.StatusSeeOther)
}
