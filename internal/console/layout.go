package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"

	"github.com/danmestas/notify/internal/domain"
)

// layout.go owns the base layout render: the template tree, the nav IA,
// the pageData every full page binds, and the shared partial types
// (page header tiles, empty state, action button). Page files add their
// own `content` overlay; partials are reachable from every tree.

// templateSet bundles the parsed templates (dagnats handler.go idiom).
// base owns layout + login + every shared partial (no `content`
// definition); pageTemplates[name] is a clone of base with one page's
// `content` overlay parsed in — the trick that lets each page own its
// own `content` template without colliding. Fragment endpoints reuse
// base so they can render any partial by name.
type templateSet struct {
	base          *template.Template
	pageTemplates map[string]*template.Template
}

// pageContentFiles maps a section key to the HTML file owning that
// section's `{{define "content"}}`. Adding a page is: add a file, add an
// entry here, add the nav entry (if any) to navGroups.
var pageContentFiles = map[string]string{
	"dashboard":         "templates/dashboard.html",
	"analytics":         "templates/analytics.html",
	"activity":          "templates/activity.html",
	"not-found":         "templates/not_found.html",
	"error":             "templates/error.html",
	"workflows":         "templates/workflows.html",
	"workflow-detail":   "templates/workflow_detail.html",
	"workflow-test":     "templates/workflow_test.html",
	"workflow-activity": "templates/workflow_activity.html",
	"integrations":      "templates/integrations.html",
	"subscribers":       "templates/subscribers.html",
	"subscriber-detail": "templates/subscriber_detail.html",
	"topics":            "templates/topics.html",
	"topic-detail":      "templates/topic_detail.html",
	"webhooks":          "templates/webhooks.html",
	// W4-V1/V2: variables CRUD.
	"variables":       "templates/variables.html",
	"variable-detail": "templates/variable_detail.html",
	// W4: contexts CRUD.
	"contexts":       "templates/contexts.html",
	"context-detail": "templates/context_detail.html",
	// W4-L1: layouts list + editor.
	"layouts":       "templates/layouts.html",
	"layout-editor": "templates/layout_editor.html",
	// W3: agents CRUD + conversation console.
	"agents":             "templates/agents.html",
	"agent-detail":       "templates/agent_detail.html",
	"agent-conversation": "templates/agent_conversation.html",
}

// loadTemplates builds the base tree and the per-page clones. Template
// funcs needing Console state (runURL) are bound later in New; the
// parse-time funcMap carries only stateless helpers.
func loadTemplates() (*templateSet, error) {
	base := template.New("console").Funcs(staticFuncMap())
	base, err := base.ParseFS(
		templatesFS,
		"templates/layout.html",
		"templates/login.html",
		"templates/partials/*.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse base templates: %w", err)
	}
	if base.Lookup("layout") == nil {
		return nil, fmt.Errorf("template tree missing `layout` template")
	}
	pages := make(map[string]*template.Template, len(pageContentFiles))
	for section, file := range pageContentFiles {
		clone, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone base for %s: %w", section, err)
		}
		clone, err = clone.ParseFS(templatesFS, file)
		if err != nil {
			return nil, fmt.Errorf("parse page %s: %w", section, err)
		}
		if clone.Lookup("content") == nil {
			return nil, fmt.Errorf("page %s missing `content` template", section)
		}
		pages[section] = clone
	}
	return &templateSet{base: base, pageTemplates: pages}, nil
}

// staticFuncMap returns the stateless template helpers. Console-bound
// helpers (runURL) are layered on in New via Console.funcMap.
func staticFuncMap() template.FuncMap {
	return template.FuncMap{
		// runURL is overridden in Console.funcMap; this default keeps
		// loadTemplates parseable in isolation (template.Funcs must know
		// every name before Parse).
		"runURL": func(string) string { return "" },
		// actionArgs packs the action-button partial's inputs so callers
		// (templates and Go views alike) bind the one actionButton shape.
		"actionArgs": func(label, href string, readOnly bool) actionButton {
			return actionButton{Label: label, Href: href, ReadOnly: readOnly}
		},
		"jsEscape": jsEscapeStringLiteral,
		"json":     jsonLiteral,
		// not negates a boolean; used in templates that cannot use "if not x"
		// (which is valid Go template syntax but can look ambiguous for readers).
		"not": func(v bool) bool { return !v },
		// list packs a variadic of strings into a slice the workflow editor
		// ranges over to render its per-type "add step" buttons.
		"list": func(items ...string) []string { return items },
		// sub subtracts b from a; the editor's "is this the last step?" guard
		// uses it to hide the move-down button on the final step.
		"sub": func(a, b int) int { return a - b },
		// dict packs alternating key/value pairs into a map so a template can
		// pass several named values into a sub-template (no struct needed).
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				if k, ok := pairs[i].(string); ok {
					m[k] = pairs[i+1]
				}
			}
			return m
		},
		// activityEmptyDetail returns an empty activityDetailView for the
		// initial panel render (no notification selected). The type is
		// defined in activity.go (same package).
		"activityEmptyDetail": func() activityDetailView {
			return activityDetailView{Empty: true}
		},
	}
}

// jsEscapeStringLiteral returns a single-quoted, JS-escaped string literal
// for s, safe to embed inside a double- or single-quoted HTML attribute
// holding a Datastar expression. See the staticFuncMap "jsEscape" comment.
func jsEscapeStringLiteral(s string) template.JS {
	return template.JS("'" + template.JSEscapeString(s) + "'")
}

// jsonLiteral marshals v to a JSON literal as template.JS. On marshal
// failure it returns a JS null literal rather than erroring the render —
// a cosmetic attribute must never 500 the page.
func jsonLiteral(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b)
}

// funcMap returns the Console-bound template helpers.
func (c *Console) funcMap() template.FuncMap {
	if c == nil {
		panic("Console.funcMap: c is nil")
	}
	return template.FuncMap{
		"runURL": c.runURL,
	}
}

// runURL resolves a dagnats run id to its engine-console deep link
// (anchor decision 3: runs/DLQ/metrics stay in the dagnats console;
// notify deep-links via the run_id join). Returns "" when the deep-link
// base (NOTIFY_DAGNATS_CONSOLE_URL) is unset or the id is empty —
// templates branch on truthiness and render plain text instead of a
// dead link. Consumed by the activity feed slice (W1-6); plumbed here.
func (c *Console) runURL(runID string) string {
	if c == nil {
		panic("Console.runURL: c is nil")
	}
	if c.deps.DagnatsConsoleURL == "" || runID == "" {
		return ""
	}
	// PathEscape the id even though dagnats run ids are opaque ULIDs today:
	// the field is DB-stored and GetByRunID is unscoped, so a future writer
	// must not be able to inject a traversal/escape sequence into the URL.
	return c.deps.DagnatsConsoleURL + "/console/runs/" + url.PathEscape(runID)
}

// navEntry is one link in the primary nav.
type navEntry struct {
	Label   string
	Href    string
	Section string
}

// navGroup is one section of the nav IA. Groups with no entries are not
// rendered (placeholder-free: no link may point at a 404). The IA is
// Dashboard · Workflows · Activity · Analytics; later slices append
// their entries in their own PRs.
type navGroup struct {
	Label   string
	Entries []navEntry
}

// navGroups is the console's primary nav inventory. Order is the render
// order.
var navGroups = []navGroup{
	{Entries: []navEntry{{Label: "Dashboard", Href: "/console/", Section: "dashboard"}}},
	{Entries: []navEntry{{Label: "Workflows", Href: "/console/workflows", Section: "workflows"}}},
	{Entries: []navEntry{{Label: "Activity", Href: "/console/activity", Section: "activity"}}},
	{Entries: []navEntry{{Label: "Analytics", Href: "/console/analytics", Section: "analytics"}}},
	{Entries: []navEntry{{Label: "Integrations", Href: "/console/integrations", Section: "integrations"}}},
	{Entries: []navEntry{{Label: "Subscribers", Href: "/console/subscribers", Section: "subscribers"}}},
	{Entries: []navEntry{{Label: "Topics", Href: "/console/topics", Section: "topics"}}},
	{Entries: []navEntry{{Label: "Webhooks", Href: "/console/webhooks", Section: "webhooks"}}},
	{Entries: []navEntry{{Label: "Variables", Href: "/console/variables", Section: "variables"}}},
	{Entries: []navEntry{{Label: "Contexts", Href: "/console/contexts", Section: "contexts"}}},
	{Entries: []navEntry{{Label: "Layouts", Href: "/console/layouts", Section: "layouts"}}},
	{Entries: []navEntry{{Label: "Agents", Href: "/console/agents", Section: "agents"}}},
}

// pageData is what the layout template binds for every full page.
type pageData struct {
	Title    string
	Section  string
	Nav      []navGroup
	Scope    domain.Scope
	CSRF     string
	ReadOnly bool
	Page     any
}

// renderPage executes section's page template into the layout and writes
// it with status. Render failures fall back to the plain-text 500 — the
// error page itself rendering is how the operator usually sees failures,
// so the fallback must not recurse.
func (c *Console) renderPage(
	w http.ResponseWriter, r *http.Request,
	status int, section, title string, page any,
) {
	if w == nil {
		panic("renderPage: w is nil")
	}
	if r == nil {
		panic("renderPage: r is nil")
	}
	tmpl, ok := c.ts.pageTemplates[section]
	if !ok {
		panic(fmt.Sprintf("renderPage: unknown section %q", section))
	}
	s, _ := sessionFromContext(r.Context())
	scope, _ := domain.ScopeFromContext(r.Context())
	data := pageData{
		Title:    title,
		Section:  section,
		Nav:      navGroups,
		Scope:    scope,
		CSRF:     s.CSRF,
		ReadOnly: c.deps.ReadOnly,
		Page:     page,
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		c.deps.Logger.Error("console: render page", "section", section, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusNotFound {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// notFoundView powers the not-found page template.
type notFoundView struct {
	Path string
}

// serveNotFound renders the layout-wrapped 404 so the operator keeps the
// chrome and a path back to the dashboard.
func (c *Console) serveNotFound(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		panic("serveNotFound: r is nil")
	}
	c.renderPage(w, r, http.StatusNotFound, "not-found", "Not found",
		notFoundView{Path: r.URL.Path})
}

// serveError renders the layout-wrapped 500 page. Handlers call it after
// logging the underlying error; no error detail reaches the browser.
func (c *Console) serveError(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		panic("serveError: r is nil")
	}
	c.renderPage(w, r, http.StatusInternalServerError, "error",
		"Something went wrong", nil)
}

// tileTone is the closed set of tone tokens a KPI tile may carry
// (dagnats page_header.go idiom). Adding a tone means editing this file
// and the CSS — intentional friction that keeps the palette uniform.
type tileTone string

const (
	toneDefault tileTone = "default"
	toneSuccess tileTone = "success"
	toneWarning tileTone = "warning"
	toneDanger  tileTone = "danger"
	toneInfo    tileTone = "info"
)

// Class maps the tone to its CSS class; called from templates.
func (t tileTone) Class() string {
	switch t {
	case toneSuccess:
		return "tile-tone-success"
	case toneWarning:
		return "tile-tone-warning"
	case toneDanger:
		return "tile-tone-danger"
	case toneInfo:
		return "tile-tone-info"
	}
	return "tile-tone-default"
}

// tile is one count cell in a page-header / KPI strip.
type tile struct {
	Label   string
	Count   int
	Tone    tileTone
	Href    string
	Tooltip string
}

// pageHeader binds the page-header partial: title row + optional
// subtitle + a strip of count tiles.
type pageHeader struct {
	Title    string
	Subtitle string
	Tiles    []tile
}

// validate enforces the construction-time contract so a typo'd tone
// surfaces in the handler's log next to the page name, not as a
// quietly-default tile.
func (h pageHeader) validate() error {
	if h.Title == "" {
		return fmt.Errorf("page header: title is empty")
	}
	const tilesMax = 16
	if len(h.Tiles) > tilesMax {
		return fmt.Errorf("page header: too many tiles: %d > %d", len(h.Tiles), tilesMax)
	}
	for i, t := range h.Tiles {
		if t.Label == "" {
			return fmt.Errorf("page header: tile %d: label is empty", i)
		}
		if t.Count < 0 {
			return fmt.Errorf("page header: tile %d: count is negative", i)
		}
		switch t.Tone {
		case toneDefault, toneSuccess, toneWarning, toneDanger, toneInfo:
		default:
			return fmt.Errorf("page header: tile %d: unknown tone %q", i, string(t.Tone))
		}
	}
	return nil
}

// emptyState binds the empty-state partial: icon glyph → title →
// description → optional primary action, the shared educational empty
// body every list page renders instead of an empty table.
type emptyState struct {
	Icon          string
	Title         string
	Description   string
	PrimaryAction *emptyStateAction
	ReadOnly      bool
}

// emptyStateAction is the optional CTA below the description.
type emptyStateAction struct {
	Label string
	Href  string
}

// actionButton binds the ReadOnly-aware action-button partial: a
// button-styled link that renders disabled (with an explanatory tooltip)
// when the console is read-only. Later slices wrap their mutation CTAs
// in it so read-only handling stays in one template.
type actionButton struct {
	Label    string
	Href     string
	ReadOnly bool
}
