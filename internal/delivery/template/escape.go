package template

import (
	"html"
	"reflect"
	"strings"

	"github.com/aymerick/raymond"
)

// Escaper turns a plain-text value into safe text for the destination's
// markup. The destination is known when a step renders, so the handler
// chooses, rather than every workflow author remembering {{{triple}}}
// braces (#364).
type Escaper func(string) string

// Plain inserts values exactly as sent. For destinations that interpret no
// markup: email subjects and text bodies, push, SMS, in-app, Discord.
func Plain(s string) string { return s }

// HTML escapes values for an HTML destination: an HTML email body, or a
// Telegram message sent with parse_mode HTML. It uses numeric entities for
// quotes, which Telegram accepts and Handlebars' &apos; is not.
func HTML(s string) string { return html.EscapeString(s) }

// Slack escapes the three characters Slack's mrkdwn reserves, and nothing
// else, so a quote or apostrophe arrives as written.
func Slack(s string) string {
	return slackEscaper.Replace(s)
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// RenderAs evaluates tmpl as a Handlebars template against data, with every
// inserted value passed through esc, and registers the {{t}} translation
// helper. The template's own text is never escaped: it is the author's
// markup. A malformed template degrades to its literal source.
//
// Values are pre-escaped and marked safe before Handlebars sees them, so
// Handlebars' built-in HTML escaping never applies. Unescaping the output
// afterwards instead would also rewrite a literal "&amp;" a user typed.
func RenderAs(esc Escaper, tmpl string, data map[string]any, translate func(key string) string) string {
	tpl, err := raymond.Parse(tmpl)
	if err != nil {
		return tmpl
	}
	tpl.RegisterHelper("t", func(key string) raymond.SafeString {
		return raymond.SafeString(esc(translate(key)))
	})
	out, err := tpl.Exec(EscapeValues(data, esc))
	if err != nil {
		return tmpl
	}
	return out
}

// Trusted marks s as already rendered for its destination, so RenderAs
// inserts it untouched. Only for notify's own output, such as a rendered
// body placed into a layout's {{content}}; never for user data.
func Trusted(s string) any { return raymond.SafeString(s) }

// escapeValues returns a copy of data in which every string is escaped with
// esc and marked safe. Maps with string keys and slices are walked, whatever
// their element type; other values pass through for Handlebars to format.
// Struct fields are not walked: template data is decoded JSON, so there are
// none, and a struct's strings would fall back to Handlebars' HTML escaping.
func EscapeValues(data map[string]any, esc Escaper) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = escapeValue(v, esc)
	}
	return out
}

func escapeValue(v any, esc Escaper) any {
	switch x := v.(type) {
	case nil:
		return nil
	case raymond.SafeString:
		return x
	case string:
		return raymond.SafeString(esc(x))
	case map[string]any:
		return EscapeValues(x, esc)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = escapeValue(e, esc)
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = escapeValue(iter.Value().Interface(), esc)
		}
		return out
	case reflect.Slice, reflect.Array:
		if (rv.Kind() == reflect.Slice && rv.IsNil()) || rv.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = escapeValue(rv.Index(i).Interface(), esc)
		}
		return out
	case reflect.String:
		return raymond.SafeString(esc(rv.String()))
	}
	return v
}
