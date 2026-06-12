package console

// webhooks.go implements W2-D2 (webhook endpoint list + CRUD) for the
// operator console.
//
// All data access is in-process via Deps.Webhooks
// (*webhooksout.ManagementService). No loopback HTTP hops. Every mutation
// handler is gated on Deps.ReadOnly server-side.

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
)

// ─── signal keys ─────────────────────────────────────────────────────────────

var webhookSignalKeys = []string{
	"webhooksFilter",
}

// ─── view structs ─────────────────────────────────────────────────────────────

// webhooksPageView is what webhooks.html binds.
type webhooksPageView struct {
	Header   pageHeader
	Fragment webhooksListView
	Signals  template.JS
	ReadOnly bool
	// ShowCreate renders the create-endpoint slide-over
	// (/console/webhooks/new).
	ShowCreate bool
}

// webhooksListView is the #webhooks-tbody fragment binding.
type webhooksListView struct {
	Rows     []webhookRow
	ReadOnly bool
	// ConfirmDelete names the endpoint ID awaiting inline two-step delete
	// confirmation (chassis idiom — no native confirm() dialogs).
	ConfirmDelete string
	// CSRF is the session token for the POST forms rendered inside the
	// fragment.
	CSRF string
}

// webhookRow is one <tr> in the webhooks table.
type webhookRow struct {
	ID     string
	URL    string
	Active bool
}

// webhooksSignalsJSON renders the initial Datastar signal envelope for the
// webhooks page. The filter signal is plumbed now so the fragment envelope
// vocabulary knows it exists (future filter UI).
func webhooksSignalsJSON(filter string) template.JS {
	sig := map[string]string{
		"webhooksFilter": filter,
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view builder ─────────────────────────────────────────────────────────────

// buildWebhooksListView reads the optional confirmDelete URL param, lists the
// scope's endpoints, and assembles the fragment binding.
func (c *Console) buildWebhooksListView(r *http.Request) (webhooksListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildWebhooksListView: no scope in context")
	}

	// The filter signal is plumbed through the shared fragment-param
	// vocabulary so the envelope knows the key exists. No server-side
	// filtering is applied yet (future filter UI, W2-D2 scope is CRUD).
	params := mergeFragmentParams(r, webhookSignalKeys...)
	filter := ""
	if vs := params["q"]; len(vs) > 0 {
		filter = strings.ToLower(strings.TrimSpace(vs[0]))
	}

	endpoints, err := c.deps.Webhooks.List(r.Context(), scope)
	if err != nil {
		return webhooksListView{}, err
	}

	rows := make([]webhookRow, 0, len(endpoints))
	for _, ep := range endpoints {
		if filter != "" && !strings.Contains(strings.ToLower(ep.URL), filter) {
			continue
		}
		rows = append(rows, webhookRow{
			ID:     ep.ID,
			URL:    ep.URL,
			Active: ep.Active,
		})
	}

	s, _ := sessionFromContext(r.Context())

	return webhooksListView{
		Rows:          rows,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		CSRF:          s.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveWebhooksPage renders GET /console/webhooks.
func (c *Console) serveWebhooksPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWebhooksPage: w is nil")
	}
	if r == nil {
		panic("serveWebhooksPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveWebhooksPage: no scope in context")
	}

	frag, err := c.buildWebhooksListView(r)
	if err != nil {
		c.deps.Logger.Error("console: webhooks page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Webhooks",
		Subtitle: "Outbound endpoints that receive delivery events.",
		Tiles: []tile{
			{Label: "endpoints", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Webhooks"}
	}

	view := webhooksPageView{
		Header:   header,
		Fragment: frag,
		Signals:  webhooksSignalsJSON(""),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "webhooks", "Webhooks", view)
}

// serveWebhookNew renders GET /console/webhooks/new with the create slide-over
// visible.
func (c *Console) serveWebhookNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveWebhookNew: w is nil")
	}
	if r == nil {
		panic("serveWebhookNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveWebhookNew: no scope in context")
	}

	frag, err := c.buildWebhooksListView(r)
	if err != nil {
		c.deps.Logger.Error("console: webhooks new list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Webhooks",
		Subtitle: "Outbound endpoints that receive delivery events.",
		Tiles: []tile{
			{Label: "endpoints", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Webhooks"}
	}

	view := webhooksPageView{
		Header:   header,
		Fragment: frag,
		Signals:  webhooksSignalsJSON(""),
		ReadOnly: c.deps.ReadOnly,
		// The create slide-over is the whole point of /new; read-only
		// consoles render the plain list instead (mutations are gated
		// server-side regardless).
		ShowCreate: !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "webhooks", "Add endpoint", view)
}

// serveFragmentWebhooksList handles GET /console/api/fragments/webhooks-list.
func (c *Console) serveFragmentWebhooksList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentWebhooksList: w is nil")
	}
	if r == nil {
		panic("serveFragmentWebhooksList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentWebhooksList: no scope in context")
	}

	view, err := c.buildWebhooksListView(r)
	if err != nil {
		c.deps.Logger.Error("console: webhooks fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "webhooks-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render webhooks fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleWebhookCreate handles POST /console/webhooks.
func (c *Console) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWebhookCreate: w is nil")
	}
	if r == nil {
		panic("handleWebhookCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWebhookCreate: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	active := r.FormValue("active") == "1"

	if _, err := c.deps.Webhooks.Create(r.Context(), scope, rawURL, active); err != nil {
		c.deps.Logger.Error("console: create webhook", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/webhooks", http.StatusSeeOther)
}

// handleWebhookUpdate handles POST /console/api/webhooks/{id}. It updates the
// URL (and optionally the active flag) then redirects back to the list page.
// Mutation responses must be plain 303s (chassis idiom, same as topics): the
// tbody forms are native POSTs, and a native form submission navigates to the
// response — an SSE body would render as raw text.
func (c *Console) handleWebhookUpdate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWebhookUpdate: w is nil")
	}
	if r == nil {
		panic("handleWebhookUpdate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWebhookUpdate: no scope in context")
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

	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	// The active flag is only updated when the form carries the field; an
	// absent field preserves the existing state (nil pointer).
	var active *bool
	if r.Form.Has("active") {
		v := r.FormValue("active") == "1"
		active = &v
	}

	if _, err := c.deps.Webhooks.Update(r.Context(), scope, id, rawURL, active); err != nil {
		c.deps.Logger.Error("console: update webhook", "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/webhooks", http.StatusSeeOther)
}

// handleWebhookToggle handles POST /console/api/webhooks/{id}/toggle. It flips
// the active flag then redirects back to the list page (303 — see
// handleWebhookUpdate for why mutations never answer SSE).
func (c *Console) handleWebhookToggle(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWebhookToggle: w is nil")
	}
	if r == nil {
		panic("handleWebhookToggle: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWebhookToggle: no scope in context")
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

	ep, err := c.deps.Webhooks.GetByID(r.Context(), scope, id)
	if err != nil {
		c.deps.Logger.Error("console: toggle webhook lookup", "err", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}

	next := !ep.Active
	if _, err := c.deps.Webhooks.Update(r.Context(), scope, id, ep.URL, &next); err != nil {
		c.deps.Logger.Error("console: toggle webhook", "err", err)
		http.Error(w, "toggle failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/webhooks", http.StatusSeeOther)
}

// handleWebhookDelete handles POST /console/api/webhooks/{id}/delete. It
// deletes the endpoint then redirects back to the list page (303 — see
// handleWebhookUpdate for why mutations never answer SSE).
func (c *Console) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWebhookDelete: w is nil")
	}
	if r == nil {
		panic("handleWebhookDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWebhookDelete: no scope in context")
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

	if err := c.deps.Webhooks.Delete(r.Context(), scope, id); err != nil {
		c.deps.Logger.Error("console: delete webhook", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/console/webhooks", http.StatusSeeOther)
}
