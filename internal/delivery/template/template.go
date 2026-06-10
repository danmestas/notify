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
// result. A malformed template (e.g. an unterminated "{{") degrades to the
// literal source rather than panicking or returning an empty string.
func Render(tmpl string, data map[string]any) string {
	out, err := raymond.Render(tmpl, data)
	if err != nil {
		return tmpl
	}
	return out
}

// RenderI18n evaluates tmpl like Render, but registers a {{t}} helper that
// resolves a key to its localized string via translate. A template without
// {{t}} renders identically to Render; a malformed template degrades to the
// literal source.
func RenderI18n(tmpl string, data map[string]any, translate func(key string) string) string {
	tpl, err := raymond.Parse(tmpl)
	if err != nil {
		return tmpl
	}
	tpl.RegisterHelper("t", func(key string) string { return translate(key) })
	out, err := tpl.Exec(data)
	if err != nil {
		return tmpl
	}
	return out
}
