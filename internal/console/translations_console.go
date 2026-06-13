package console

// translations_console.go implements W4-L2 (translations list + locale editor
// + settings panel) for the operator console.
//
// All data access is in-process via Deps.Translations (store.TranslationRepo)
// and Deps.Environments (store.EnvironmentRepo). No loopback HTTP hops. Every
// mutation handler is gated on Deps.ReadOnly server-side; mutations answer with
// a 303 redirect (never SSE to a native form POST).

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ─── signals ─────────────────────────────────────────────────────────────────

// translationsSignalsJSON renders the initial Datastar signal envelope for the
// translations pages: translationContent binds the locale-editor textarea so
// the operator's edits survive a fragment swap.
func translationsSignalsJSON(content string) template.JS {
	sig := map[string]string{"translationContent": content}
	b, err := json.Marshal(sig)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// ─── view structs ─────────────────────────────────────────────────────────────

// translationsPageView is what translations.html binds.
type translationsPageView struct {
	Header   pageHeader
	Fragment translationsListView
	Signals  template.JS
	ReadOnly bool
	// ShowCreate renders the add-locale slide-over (/console/translations/new).
	ShowCreate bool
}

// translationsListView is the #translations-tbody fragment binding.
type translationsListView struct {
	Rows []translationLocaleRow
	// ConfirmDelete names the locale awaiting inline two-step delete.
	ConfirmDelete string
	ReadOnly      bool
	CSRF          string
}

// translationLocaleRow is one <tr> in the locales table.
type translationLocaleRow struct {
	Locale   string
	KeyCount int
}

// localeEditorView is what translation_editor.html binds.
type localeEditorView struct {
	Header        pageHeader
	Locale        string
	Content       string // JSON-encoded map[string]string
	Signals       template.JS
	ReadOnly      bool
	ConfirmDelete bool
	FormError     string
	CSRF          string
}

// translationSettingsView is what translation_settings.html binds.
type translationSettingsView struct {
	Header        pageHeader
	DefaultLocale string
	TargetLocales string // comma-separated for display
	ReadOnly      bool
	FormError     string
	CSRF          string
}

// ─── view builder ─────────────────────────────────────────────────────────────

// buildTranslationsListView lists the scope's locales with key counts and
// assembles the fragment binding.
func (c *Console) buildTranslationsListView(r *http.Request) (translationsListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildTranslationsListView: no scope in context")
	}
	stats, err := c.deps.Translations.LocaleStats(r.Context(), scope)
	if err != nil {
		return translationsListView{}, err
	}
	rows := make([]translationLocaleRow, 0, len(stats))
	for _, s := range stats {
		rows = append(rows, translationLocaleRow{Locale: s.Locale, KeyCount: s.KeyCount})
	}
	sess, _ := sessionFromContext(r.Context())
	return translationsListView{
		Rows:          rows,
		ConfirmDelete: r.URL.Query().Get("confirmDelete"),
		ReadOnly:      c.deps.ReadOnly,
		CSRF:          sess.CSRF,
	}, nil
}

// ─── page handlers ────────────────────────────────────────────────────────────

// serveTranslationsPage renders GET /console/translations.
func (c *Console) serveTranslationsPage(w http.ResponseWriter, r *http.Request) {
	c.renderTranslationsPage(w, r, false)
}

// serveTranslationsNew renders GET /console/translations/new with the
// add-locale slide-over visible.
func (c *Console) serveTranslationsNew(w http.ResponseWriter, r *http.Request) {
	c.renderTranslationsPage(w, r, true)
}

// renderTranslationsPage is the shared body of the list and add-slide-over
// pages; showCreate toggles the slide-over.
func (c *Console) renderTranslationsPage(w http.ResponseWriter, r *http.Request, showCreate bool) {
	if w == nil {
		panic("renderTranslationsPage: w is nil")
	}
	if r == nil {
		panic("renderTranslationsPage: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("renderTranslationsPage: no scope in context")
	}

	frag, err := c.buildTranslationsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: translations page list", "err", err)
		c.serveError(w, r)
		return
	}

	header := pageHeader{
		Title:    "Translations",
		Subtitle: "Localized message strings, grouped by locale.",
		Tiles: []tile{
			{Label: "locales", Count: len(frag.Rows), Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Translations"}
	}

	title := "Translations"
	if showCreate {
		title = "Add locale"
	}
	view := translationsPageView{
		Header:     header,
		Fragment:   frag,
		Signals:    translationsSignalsJSON(""),
		ReadOnly:   c.deps.ReadOnly,
		ShowCreate: showCreate && !c.deps.ReadOnly,
	}
	c.renderPage(w, r, http.StatusOK, "translations", title, view)
}

// serveFragmentTranslationsList handles
// GET /console/api/fragments/translations-list.
func (c *Console) serveFragmentTranslationsList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentTranslationsList: w is nil")
	}
	if r == nil {
		panic("serveFragmentTranslationsList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentTranslationsList: no scope in context")
	}

	view, err := c.buildTranslationsListView(r)
	if err != nil {
		c.deps.Logger.Error("console: translations fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}

	html, err := renderFragment(c.ts.base, "translations-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render translations fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// serveLocaleEditor renders GET /console/translations/{locale}.
func (c *Console) serveLocaleEditor(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveLocaleEditor: w is nil")
	}
	if r == nil {
		panic("serveLocaleEditor: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveLocaleEditor: no scope in context")
	}

	locale := chi.URLParam(r, "locale")
	m, err := c.deps.Translations.Map(r.Context(), scope, locale)
	if err != nil {
		c.deps.Logger.Error("console: locale editor map", "err", err)
		c.serveError(w, r)
		return
	}
	content := prettyJSONMap(m)

	header := pageHeader{
		Title:    "Edit locale",
		Subtitle: locale,
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Edit locale"}
	}

	sess, _ := sessionFromContext(r.Context())
	view := localeEditorView{
		Header:        header,
		Locale:        locale,
		Content:       content,
		Signals:       translationsSignalsJSON(content),
		ReadOnly:      c.deps.ReadOnly,
		ConfirmDelete: r.URL.Query().Get("confirmDelete") == "true",
		CSRF:          sess.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "translation-editor", "Edit locale", view)
}

// serveTranslationsSettings renders GET /console/translations/settings.
func (c *Console) serveTranslationsSettings(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveTranslationsSettings: w is nil")
	}
	if r == nil {
		panic("serveTranslationsSettings: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveTranslationsSettings: no scope in context")
	}

	var (
		defaultLocale string
		targets       []string
	)
	if c.deps.Environments != nil {
		s, err := c.deps.Environments.GetTranslationSettings(r.Context(), scope)
		if err != nil {
			c.deps.Logger.Error("console: get translation settings", "err", err)
			c.serveError(w, r)
			return
		}
		defaultLocale = s.DefaultLocale
		targets = s.TargetLocales
	}

	header := pageHeader{
		Title:    "Translation settings",
		Subtitle: "Default and target locales for this environment.",
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Translation settings"}
	}

	sess, _ := sessionFromContext(r.Context())
	view := translationSettingsView{
		Header:        header,
		DefaultLocale: defaultLocale,
		TargetLocales: strings.Join(targets, ", "),
		ReadOnly:      c.deps.ReadOnly || c.deps.Environments == nil,
		CSRF:          sess.CSRF,
	}
	c.renderPage(w, r, http.StatusOK, "translation-settings", "Translation settings", view)
}

// ─── mutation handlers ────────────────────────────────────────────────────────

// handleTranslationLocaleCreate handles POST /console/api/translations. The
// locale is "created" lazily — its first key save (BulkReplace) materializes
// the rows — so this handler validates the code and redirects to the editor.
func (c *Console) handleTranslationLocaleCreate(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTranslationLocaleCreate: w is nil")
	}
	if r == nil {
		panic("handleTranslationLocaleCreate: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("handleTranslationLocaleCreate: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	locale := strings.TrimSpace(r.FormValue("locale"))
	if locale == "" {
		http.Error(w, "locale is required", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/console/translations/"+locale, http.StatusSeeOther)
}

// handleTranslationLocaleSave handles
// POST /console/api/translations/{locale}/save. It parses the form's content
// field (a JSON object) and bulk-replaces the locale's key map.
func (c *Console) handleTranslationLocaleSave(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTranslationLocaleSave: w is nil")
	}
	if r == nil {
		panic("handleTranslationLocaleSave: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTranslationLocaleSave: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	locale := chi.URLParam(r, "locale")
	content := r.FormValue("content")

	pairs := map[string]string{}
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &pairs); err != nil {
			c.renderLocaleEditorError(w, r, locale, content, "Content must be a valid JSON object of key/value strings.")
			return
		}
	}

	if err := c.deps.Translations.BulkReplace(r.Context(), scope, locale, pairs); err != nil {
		c.deps.Logger.Error("console: save locale", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/translations/"+locale, http.StatusSeeOther)
}

// renderLocaleEditorError re-renders the editor with an inline parse error and
// the operator's unsaved content preserved.
func (c *Console) renderLocaleEditorError(w http.ResponseWriter, r *http.Request, locale, content, msg string) {
	header := pageHeader{Title: "Edit locale", Subtitle: locale}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Edit locale"}
	}
	sess, _ := sessionFromContext(r.Context())
	view := localeEditorView{
		Header:    header,
		Locale:    locale,
		Content:   content,
		Signals:   translationsSignalsJSON(content),
		ReadOnly:  c.deps.ReadOnly,
		FormError: msg,
		CSRF:      sess.CSRF,
	}
	c.renderPage(w, r, http.StatusBadRequest, "translation-editor", "Edit locale", view)
}

// handleTranslationLocaleDelete handles
// POST /console/api/translations/{locale}/delete.
func (c *Console) handleTranslationLocaleDelete(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTranslationLocaleDelete: w is nil")
	}
	if r == nil {
		panic("handleTranslationLocaleDelete: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTranslationLocaleDelete: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	locale := chi.URLParam(r, "locale")
	if err := c.deps.Translations.DeleteLocale(r.Context(), scope, locale); err != nil {
		// A locale with no rows is already "gone": treat ErrNotFound as a
		// successful no-op so the two-step delete stays idempotent.
		if !errors.Is(err, store.ErrNotFound) {
			c.deps.Logger.Error("console: delete locale", "err", err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/console/translations", http.StatusSeeOther)
}

// handleTranslationSettingsSave handles POST /console/api/translations/settings.
func (c *Console) handleTranslationSettingsSave(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleTranslationSettingsSave: w is nil")
	}
	if r == nil {
		panic("handleTranslationSettingsSave: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleTranslationSettingsSave: no scope in context")
	}

	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.Environments == nil {
		http.Error(w, "settings unavailable", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	defaultLocale := strings.TrimSpace(r.FormValue("default_locale"))
	targets := parseCommaList(r.FormValue("target_locales"))

	if err := c.deps.Environments.SetTranslationSettings(r.Context(), scope, store.TranslationSettings{
		DefaultLocale: defaultLocale,
		TargetLocales: targets,
	}); err != nil {
		c.deps.Logger.Error("console: save translation settings", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/console/translations/settings", http.StatusSeeOther)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// prettyJSONMap renders m as a stable, indented JSON object suitable for the
// editor textarea. Keys are sorted so the textarea is deterministic.
func prettyJSONMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{\n")
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		b.WriteString("  ")
		b.Write(kb)
		b.WriteString(": ")
		b.Write(vb)
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}")
	return b.String()
}

// parseCommaList splits a comma-separated string into trimmed, non-empty
// elements (e.g. "es, fr, " → ["es","fr"]).
func parseCommaList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
