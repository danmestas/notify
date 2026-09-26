// Package template renders Handlebars templates.
//
// It is a thin boundary over github.com/aymerick/raymond, exposing the full
// Handlebars feature set (helpers, #if/#each/#with, partials) behind RenderAs,
// which escapes inserted values for the destination (escape.go). Dotted paths
// like {{subscriber.firstName}} resolve against map[string]any and structs,
// numbers/bools render via their natural string form, and missing paths render
// as "".
package template
