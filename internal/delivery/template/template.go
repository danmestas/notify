// Package template renders Handlebars templates.
//
// It is a thin boundary over github.com/aymerick/raymond, exposing the full
// Handlebars feature set (helpers, #if/#each/#with, partials) while keeping the
// simple Render(tmpl, data) signature delivery handlers depend on. Dotted paths
// like {{subscriber.firstName}} resolve against map[string]any and structs,
// numbers/bools render via their natural string form, and missing paths render
// as "".
package template

import "github.com/aymerick/raymond"

// Render evaluates tmpl as a Handlebars template against data and returns the
// result, HTML-escaping every inserted value (Handlebars' default). A
// malformed template (e.g. an unterminated "{{") degrades to the literal
// source rather than panicking or returning an empty string.
//
// Delivery steps use RenderAs, which escapes for the destination. Render
// remains for the HTTP request step, whose values land in a URL, headers,
// and a body that each need their own escaping; that is tracked separately.
func Render(tmpl string, data map[string]any) string {
	out, err := raymond.Render(tmpl, data)
	if err != nil {
		return tmpl
	}
	return out
}
