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
	body, err := fs.ReadFile(iconFS, path.Join("assets/icons", id+".svg"))
	if err != nil {
		return providerIconPlaceholder
	}
	return template.HTML(body) //nolint:gosec // trusted vendored SVG, no user input
}

// providerIconPlaceholder is the neutral fallback rendered when a provider id
// has no embedded icon. It carries no brand colour and is aria-hidden.
const providerIconPlaceholder template.HTML = `<span class="integration-provider-icon-fallback" aria-hidden="true"></span>`
