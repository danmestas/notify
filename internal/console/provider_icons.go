package console

import (
	"html/template"
	"io/fs"
	"path"
	"strings"
)

// providerIcon returns the embedded brand SVG for a provider id as trusted
// markup ready to inline in a template. The icons are our own vendored,
// single-path SVGs (assets/icons/<id>.svg) — there is no user input in the
// bytes — so returning template.HTML is safe.
//
// On a missing or unreadable file it returns a neutral placeholder span
// instead of erroring, so an unmapped id can never break a render.
func providerIcon(id string) template.HTML {
	// Guard against path traversal: an id is only ever a catalog key.
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return providerIconPlaceholder
	}
	// Provider ids are inconsistent about separators between the console catalog
	// ("cloudflare_email") and the stored ProviderID ("cloudflare-email"), so try
	// the id verbatim first, then with separators swapped, before giving up.
	for _, name := range iconCandidates(id) {
		if body, err := fs.ReadFile(iconFS, path.Join("assets/icons", name+".svg")); err == nil {
			return template.HTML(body) //nolint:gosec // trusted vendored SVG, no user input
		}
	}
	return providerIconPlaceholder
}

// iconCandidates returns the icon filenames to try for a provider id: the id
// itself, then the hyphen/underscore-swapped variants (deduplicated).
func iconCandidates(id string) []string {
	out := []string{id}
	for _, alt := range []string{strings.ReplaceAll(id, "-", "_"), strings.ReplaceAll(id, "_", "-")} {
		if alt != id && alt != out[len(out)-1] {
			out = append(out, alt)
		}
	}
	return out
}

// providerIconPlaceholder is the neutral fallback rendered when a provider id
// has no embedded icon. It carries no brand colour and is aria-hidden.
const providerIconPlaceholder template.HTML = `<span class="integration-provider-icon-fallback" aria-hidden="true"></span>`

// uiIcon returns the embedded line-icon SVG for a UI affordance name as trusted
// markup ready to inline in a template. The icons are our own vendored,
// single-element currentColor SVGs (assets/ui-icons/<name>.svg) — there is no
// user input in the bytes — so returning template.HTML is safe.
//
// They replace the emoji and typographic-glyph "icons" the console must never
// render. Because the stroke is currentColor, an inlined icon inherits the
// surrounding text colour and adapts to the active theme automatically.
//
// On a missing or unreadable name it returns a neutral placeholder span instead
// of erroring, so an unmapped name can never break a render.
func uiIcon(name string) template.HTML {
	// Guard against path traversal: a name is only ever an icon key.
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return uiIconPlaceholder
	}
	if body, err := fs.ReadFile(uiIconFS, path.Join("assets/ui-icons", name+".svg")); err == nil {
		return template.HTML(body) //nolint:gosec // trusted vendored SVG, no user input
	}
	return uiIconPlaceholder
}

// uiIconPlaceholder is the neutral fallback rendered when a ui-icon name has no
// embedded SVG. It carries no glyph and is aria-hidden.
const uiIconPlaceholder template.HTML = `<span class="ui-icon ui-icon-fallback" aria-hidden="true"></span>`
