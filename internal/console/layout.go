package console

import (
	"bytes"
	"context"
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
	// W4-L2: translations list + locale editor + settings.
	"translations":         "templates/translations.html",
	"translation-editor":   "templates/translation_editor.html",
	"translation-settings": "templates/translation_settings.html",
	// W4-D: inbound domains.
	"domains":       "templates/domains.html",
	"domain-detail": "templates/domain_detail.html",
	// W3: agents CRUD + conversation console.
	"agents":             "templates/agents.html",
	"agent-detail":       "templates/agent_detail.html",
	"agent-conversation": "templates/agent_conversation.html",
	// W4-A5: global conversations list.
	"conversations": "templates/conversations.html",
	// W4-E1/E2: environments + API keys.
	"environments": "templates/environments.html",
	"api-keys":     "templates/api_keys.html",
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
		// navIcon renders the inline line-SVG glyph for a sidebar entry by
		// key (navEntry.Icon). Unknown keys fall back to a neutral dot so a
		// new nav entry never renders a broken icon.
		"navIcon": navIconSVG,
		// mkDefaultChannels returns the ordered list of known channel names
		// for the subscriber Preferences tab empty-state (W4-S1).
		"mkDefaultChannels": func() []string {
			return []string{"in_app", "email", "sms", "push"}
		},
	}
}

// navIconSVG maps a navEntry.Icon key to a 16x16 stroked line-icon. The
// glyphs are hand-trimmed to match the MagicPath sidebar's feel (Lucide /
// Feather family). currentColor + a shared viewBox keep them inheriting the
// link colour and sizing from .console-nav-icon in app.css.
func navIconSVG(key string) template.HTML {
	const open = `<svg class="console-nav-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">`
	const closeSVG = `</svg>`
	paths := map[string]string{
		"dashboard":     `<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/>`,
		"workflows":     `<circle cx="6" cy="6" r="2"/><circle cx="6" cy="18" r="2"/><circle cx="18" cy="6" r="2"/><path d="M8 6h6a4 4 0 0 1 4 4v0M6 8v8"/>`,
		"activity":      `<path d="M3 12h4l3 8 4-16 3 8h4"/>`,
		"subscribers":   `<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/>`,
		"topics":        `<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>`,
		"integrations":  `<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><path d="M7 14v2a2 2 0 0 0 2 2h2M14 7h2a2 2 0 0 1 2 2v2"/>`,
		"webhooks":      `<path d="M18 16.98a4 4 0 1 0-2.5-7.4M6.5 9.5A4 4 0 1 0 9 16.98M12 8v4l3 3"/>`,
		"agents":        `<rect x="4" y="8" width="16" height="12" rx="2"/><path d="M12 2v4M9 13h.01M15 13h.01M2 14h2M20 14h2"/>`,
		"conversations": `<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>`,
		"analytics":     `<path d="M3 3v18h18"/><rect x="7" y="11" width="3" height="6"/><rect x="13" y="7" width="3" height="10"/>`,
		"variables":     `<path d="M4 7V4h16v3M9 20h6M12 4v16"/>`,
		"contexts":      `<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6M9 13h6M9 17h6"/>`,
		"layouts":       `<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M9 21V9"/>`,
		"translations":  `<path d="M4 5h7M9 3v2c0 4-2 7-5 9M5 9c0 3 3 5 7 5"/><path d="M14 19l3-7 3 7M14.5 17h5"/>`,
		"domains":       `<circle cx="12" cy="12" r="10"/><path d="M2 12h20M12 2a15 15 0 0 1 0 20 15 15 0 0 1 0-20z"/>`,
		"environments":  `<path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>`,
		"api-keys":      `<rect x="3" y="11" width="18" height="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>`,
	}
	p, ok := paths[key]
	if !ok {
		p = `<circle cx="12" cy="12" r="3"/>`
	}
	return template.HTML(open + p + closeSVG) //nolint:gosec // static glyph set, no user input
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

// navEntry is one link in the sidebar nav. Icon is a key into the
// navIcon template helper (inline line-SVG glyphs matching the mockup);
// BadgeKey, when non-empty, selects a live count from pageData.NavCounts
// rendered as a pill on the right of the entry.
type navEntry struct {
	Label    string
	Href     string
	Section  string
	Icon     string
	BadgeKey string
}

// navGroup is one section of the sidebar IA. Label is the small
// uppercase section header (NOTIFY / CHANNELS / AGENTS / SYSTEM in the
// MagicPath mockup). Groups with no entries are not rendered
// (placeholder-free: no link may point at a 404).
type navGroup struct {
	Label   string
	Entries []navEntry
}

// navGroups is the console's sidebar inventory grouped to match the
// MagicPath mockup. Order is the render order. Every Href points at a
// real mounted route — the mockup's aspirational "Conversations",
// "API Keys", and "Settings" entries are omitted rather than rendered as
// dead links; their slots return when the routes ship.
var navGroups = []navGroup{
	{Label: "NOTIFY", Entries: []navEntry{
		{Label: "Dashboard", Href: "/console/", Section: "dashboard", Icon: "dashboard"},
		{Label: "Workflows", Href: "/console/workflows", Section: "workflows", Icon: "workflows", BadgeKey: "workflows"},
		{Label: "Activity", Href: "/console/activity", Section: "activity", Icon: "activity"},
		{Label: "Subscribers", Href: "/console/subscribers", Section: "subscribers", Icon: "subscribers"},
		{Label: "Topics", Href: "/console/topics", Section: "topics", Icon: "topics"},
	}},
	{Label: "CHANNELS", Entries: []navEntry{
		{Label: "Integrations", Href: "/console/integrations", Section: "integrations", Icon: "integrations", BadgeKey: "integrations"},
		{Label: "Webhooks", Href: "/console/webhooks", Section: "webhooks", Icon: "webhooks"},
	}},
	{Label: "AGENTS", Entries: []navEntry{
		{Label: "Agents", Href: "/console/agents", Section: "agents", Icon: "agents", BadgeKey: "agents"},
		{Label: "Conversations", Href: "/console/conversations", Section: "conversations", Icon: "conversations"},
	}},
	{Label: "SYSTEM", Entries: []navEntry{
		{Label: "Analytics", Href: "/console/analytics", Section: "analytics", Icon: "analytics"},
		{Label: "Variables", Href: "/console/variables", Section: "variables", Icon: "variables"},
		{Label: "Contexts", Href: "/console/contexts", Section: "contexts", Icon: "contexts"},
		{Label: "Layouts", Href: "/console/layouts", Section: "layouts", Icon: "layouts"},
		{Label: "Translations", Href: "/console/translations", Section: "translations", Icon: "translations"},
		{Label: "Domains", Href: "/console/domains", Section: "domains", Icon: "domains"},
	}},
	{Label: "SETTINGS", Entries: []navEntry{
		{Label: "Environments", Href: "/console/settings/environments", Section: "environments", Icon: "environments"},
		{Label: "API Keys", Href: "/console/settings/api-keys", Section: "api-keys", Icon: "api-keys"},
	}},
}

// consoleVersion is the build label shown in the sidebar footer
// ("notify v0.9.2 · :3000" in the mockup). Static for now; a future
// slice can plumb the real ldflags version + bound port.
const consoleVersion = "v0.9.2"

// pageData is what the layout template binds for every full page.
type pageData struct {
	Title      string
	Section    string
	Nav        []navGroup
	NavCounts  map[string]int // sidebar badge counts keyed by navEntry.BadgeKey
	Scope      domain.Scope
	CSRF       string
	ReadOnly   bool
	Version    string // sidebar footer build label
	DagnatsURL string // sidebar "dagnats console ↗" external link target ("" hides the link target)
	Page       any
}

// navCounts computes the sidebar badge counts (workflows / integrations /
// agents) for scope. Each lookup degrades to 0 on error or a nil service
// so the chrome always renders — a missing count is a blank badge, never
// a failed page. Cheap List calls over the current small datasets; if
// these grow, swap to dedicated COUNT queries.
func (c *Console) navCounts(ctx context.Context, scope domain.Scope) map[string]int {
	counts := map[string]int{}
	if c.deps.Authoring != nil {
		if wfs, err := c.deps.Authoring.List(ctx, scope); err == nil {
			counts["workflows"] = len(wfs)
		}
	}
	if c.deps.Integrations != nil {
		if ints, err := c.deps.Integrations.List(ctx, scope); err == nil {
			counts["integrations"] = len(ints)
		}
	}
	if c.deps.Agents != nil {
		if ags, err := c.deps.Agents.Agents().List(ctx, scope); err == nil {
			counts["agents"] = len(ags)
		}
	}
	return counts
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
		Title:      title,
		Section:    section,
		Nav:        navGroups,
		NavCounts:  c.navCounts(r.Context(), scope),
		Scope:      scope,
		CSRF:       s.CSRF,
		ReadOnly:   c.deps.ReadOnly,
		Version:    consoleVersion,
		DagnatsURL: c.deps.DagnatsConsoleURL,
		Page:       page,
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
