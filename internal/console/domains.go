package console

// domains.go implements W4-D2 (inbound domains list + create sheet + detail
// with MX instructions + verify action + two-step delete) for the operator
// console.
//
// All data access is in-process via Deps.InboundDomains. Every mutation is
// gated on Deps.ReadOnly server-side.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

var domainSignalKeys = []string{"domainsSearch"}

type domainsPageView struct {
	Header   pageHeader
	Fragment domainsListView
	Signals  template.JS
	ReadOnly bool
}

type domainsListView struct {
	Rows     []domainRow
	ReadOnly bool
	CSRF     string
}

type domainRow struct {
	Name         string
	Status       string
	MXConfigured bool
}

type domainDetailView struct {
	Header        pageHeader
	Dom           domain.InboundDomain
	ReadOnly      bool
	ConfirmDelete bool
	CSRF          string
	Signals       template.JS
}

func domainsSignalsJSON(search string, newOpen bool) template.JS {
	m := map[string]any{
		"domainsSearch": search,
		// domainNewOpen drives the create slide-over's data-show. Must be a
		// real JSON boolean — a string "false" is truthy in JS/Datastar.
		"domainNewOpen": newOpen,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

func (c *Console) buildDomainsListView(r *http.Request) (domainsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildDomainsListView: no scope in context")
	}
	params := mergeFragmentParams(r, domainSignalKeys...)
	search := ""
	if v := params["domainsSearch"]; len(v) > 0 {
		search = v[0]
	}
	items, _, _, err := c.deps.InboundDomains.List(r.Context(), scope, search, "", 50)
	if err != nil {
		return domainsListView{}, err
	}
	rows := make([]domainRow, 0, len(items))
	for _, d := range items {
		rows = append(rows, domainRow{
			Name:         d.Name,
			Status:       d.Status,
			MXConfigured: d.MXConfigured,
		})
	}
	s, _ := sessionFromContext(r.Context())
	return domainsListView{
		Rows:     rows,
		ReadOnly: c.deps.ReadOnly,
		CSRF:     s.CSRF,
	}, nil
}

func (c *Console) serveDomainsPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveDomainsPage: w is nil")
	}
	if r == nil {
		panic("serveDomainsPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveDomainsPage: no scope in context")
	}
	frag, err := c.buildDomainsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: domains page list", "err", err)
		c.serveError(w, r)
		return
	}
	header := pageHeader{
		Title:    "Inbound Domains",
		Subtitle: "Domains configured for inbound email reception.",
		Tiles:    []tile{{Label: "domains", Count: len(frag.Rows), Tone: toneDefault}},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Inbound Domains"}
	}
	view := domainsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  domainsSignalsJSON("", false),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "domains", "Inbound Domains", view)
}

func (c *Console) serveDomainNew(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveDomainNew: w is nil")
	}
	if r == nil {
		panic("serveDomainNew: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveDomainNew: no scope in context")
	}
	frag, err := c.buildDomainsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: domain new list", "err", err)
		c.serveError(w, r)
		return
	}
	header := pageHeader{
		Title:    "Inbound Domains",
		Subtitle: "Domains configured for inbound email reception.",
		Tiles:    []tile{{Label: "domains", Count: len(frag.Rows), Tone: toneDefault}},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Inbound Domains"}
	}
	view := domainsPageView{
		Header:   header,
		Fragment: frag,
		Signals:  domainsSignalsJSON("", !c.deps.ReadOnly),
		ReadOnly: c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "domains", "Inbound Domains", view)
}

func (c *Console) serveDomainDetail(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveDomainDetail: w is nil")
	}
	if r == nil {
		panic("serveDomainDetail: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveDomainDetail: no scope in context")
	}
	name := chi.URLParam(r, "name")
	d, err := c.deps.InboundDomains.Get(r.Context(), scope, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: domain detail", "err", err)
		c.serveError(w, r)
		return
	}
	s, _ := sessionFromContext(r.Context())
	view := domainDetailView{
		Header: pageHeader{
			Title:    d.Name,
			Subtitle: "Inbound domain MX configuration and verification.",
		},
		Dom:           d,
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		CSRF:          s.CSRF,
		Signals:       domainsSignalsJSON("", false),
	}
	if err := view.Header.validate(); err != nil {
		view.Header = pageHeader{Title: d.Name}
	}
	c.renderPage(w, r, http.StatusOK, "domain-detail", d.Name, view)
}

func (c *Console) serveFragmentDomainsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentDomainsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentDomainsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentDomainsList: no scope in context")
	}
	view, err := c.buildDomainsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: domains fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	html, err := renderFragment(c.ts.base, "domains-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render domains fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

func (c *Console) handleDomainCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleDomainCreate: w is nil")
	}
	if r == nil {
		panic("handleDomainCreate: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleDomainCreate: no scope in context")
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
	if _, err := c.deps.InboundDomains.Create(r.Context(), scope, name); err != nil {
		c.deps.Logger.Error("console: create domain", "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/domains/"+name, http.StatusSeeOther)
}

func (c *Console) handleDomainVerify(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleDomainVerify: w is nil")
	}
	if r == nil {
		panic("handleDomainVerify: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleDomainVerify: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	name := chi.URLParam(r, "name")
	d, err := c.deps.InboundDomains.Verify(r.Context(), scope, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: verify domain", "err", err)
		http.Error(w, "verify failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/console/domains/%s?verifyResult=%s", name, d.Status), http.StatusSeeOther)
}

func (c *Console) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleDomainDelete: w is nil")
	}
	if r == nil {
		panic("handleDomainDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleDomainDelete: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	name := chi.URLParam(r, "name")
	if err := c.deps.InboundDomains.Delete(r.Context(), scope, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: delete domain", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/domains", http.StatusSeeOther)
}
