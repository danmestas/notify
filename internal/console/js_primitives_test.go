package console

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"strings"
	"testing"
)

// js_primitives_test.go proves the two chassis JS-context primitives —
// jsEscapeStringLiteral ("jsEscape" in staticFuncMap) and jsonLiteral ("json")
// — round-trip a hostile value through both a direct call and a full
// html/template attribute render without breaking out of the emitted literal
// or corrupting the attribute (issue #61's correctness bar).

// jsCorrectnessBarCases are the characters/sequences the correctness bar
// calls out by name: single quote, double quote, backslash, newline,
// </script>, and non-ASCII.
var jsCorrectnessBarCases = []struct {
	name string
	in   string
}{
	{"single_quote", `it's a test`},
	{"double_quote", `she said "hi"`},
	{"backslash", `C:\path\to\file`},
	{"newline", "line one\nline two"},
	{"script_breakout", `</script><script>alert(1)</script>`},
	{"non_ascii", "héllo wörld 日本語 🎉"},
	{"mixed", "a'b\"c\\d\ne</script>f"},
}

// decodeJSSingleQuoted reverses the escape sequences jsEscapeStringLiteral
// (backed by template.JSEscapeString) produces, so a test can assert the
// escaped literal round-trips to the original value instead of merely
// eyeballing the escaped form.
func decodeJSSingleQuoted(t *testing.T, quoted string) string {
	t.Helper()
	if len(quoted) < 2 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
		t.Fatalf("not a single-quoted JS literal: %s", quoted)
	}
	body := quoted[1 : len(quoted)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			t.Fatalf("dangling escape in %s", quoted)
		}
		switch body[i] {
		case '\'':
			b.WriteByte('\'')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'u':
			if i+4 >= len(body) {
				t.Fatalf("truncated unicode escape in %s", quoted)
			}
			var r rune
			if _, err := fmt.Sscanf(body[i+1:i+5], "%04x", &r); err != nil {
				t.Fatalf("bad unicode escape in %s: %v", quoted, err)
			}
			b.WriteRune(r)
			i += 4
		default:
			t.Fatalf("unexpected escape \\%c in %s", body[i], quoted)
		}
	}
	return b.String()
}

// TestJSEscapeStringLiteral_RoundTrip calls jsEscapeStringLiteral directly
// and decodes the emitted JS literal back to the original value.
func TestJSEscapeStringLiteral_RoundTrip(t *testing.T) {
	for _, tc := range jsCorrectnessBarCases {
		t.Run(tc.name, func(t *testing.T) {
			lit := string(jsEscapeStringLiteral(tc.in))
			got := decodeJSSingleQuoted(t, lit)
			if got != tc.in {
				t.Fatalf("round-trip mismatch: got %q want %q (literal=%s)", got, tc.in, lit)
			}
		})
	}
}

// TestJSEscapeStringLiteral_SurvivesAttributeEncoding renders jsEscape (as
// registered in staticFuncMap) into a data-on:click attribute exactly the way
// every migrated template does, simulates the browser's HTML-attribute
// decode (which happens before Datastar ever parses the JS), and proves the
// decoded expression's string literal still evaluates to the original value.
func TestJSEscapeStringLiteral_SurvivesAttributeEncoding(t *testing.T) {
	tmpl := template.Must(template.New("t").Funcs(staticFuncMap()).Parse(
		`<button data-on:click="go({{ . | jsEscape }})"></button>`,
	))
	for _, tc := range jsCorrectnessBarCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			if err := tmpl.Execute(&buf, tc.in); err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			attr := extractBetween(t, out, `data-on:click="`, `"></button>`)
			decoded := html.UnescapeString(attr)
			// The escaped payload itself may contain parentheses (e.g. the
			// script_breakout case embeds "alert(1)"), so locate the JS
			// string literal by its outer single quotes rather than by the
			// call's closing paren.
			litStart := strings.Index(decoded, "'")
			litEnd := strings.LastIndex(decoded, "'")
			if litStart < 0 || litEnd <= litStart {
				t.Fatalf("could not locate JS literal in decoded expression: %s", decoded)
			}
			lit := decoded[litStart : litEnd+1]
			got := decodeJSSingleQuoted(t, lit)
			if got != tc.in {
				t.Fatalf("attribute round-trip mismatch: got %q want %q\nraw=%s\ndecoded=%s", got, tc.in, out, decoded)
			}
		})
	}
}

// TestJSONLiteral_RoundTrip proves jsonLiteral ("json" in staticFuncMap) — the
// shared "typed Go value -> JSON template.JS" primitive backing Datastar
// signals objects — produces valid JSON that round-trips a hostile value.
func TestJSONLiteral_RoundTrip(t *testing.T) {
	for _, tc := range jsCorrectnessBarCases {
		t.Run(tc.name, func(t *testing.T) {
			js := jsonLiteral(map[string]string{"value": tc.in})
			if !json.Valid([]byte(js)) {
				t.Fatalf("jsonLiteral produced invalid JSON: %s", js)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(js), &got); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, js)
			}
			if got["value"] != tc.in {
				t.Fatalf("round-trip mismatch: got %q want %q", got["value"], tc.in)
			}
		})
	}
}

// TestJSONLiteral_SurvivesAttributeEncoding renders json (as registered in
// staticFuncMap) into a data-signals attribute, simulates the browser's
// HTML-attribute decode, and proves the decoded text is still valid JSON
// that round-trips the hostile value — the exact shape every migrated
// data-signals attribute now uses instead of raw interpolation.
func TestJSONLiteral_SurvivesAttributeEncoding(t *testing.T) {
	tmpl := template.Must(template.New("t").Funcs(staticFuncMap()).Parse(
		`<div data-signals="{{ . | json }}"></div>`,
	))
	for _, tc := range jsCorrectnessBarCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			v := map[string]string{"value": tc.in}
			if err := tmpl.Execute(&buf, v); err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			attr := extractBetween(t, out, `data-signals="`, `"></div>`)
			decoded := html.UnescapeString(attr)
			if !json.Valid([]byte(decoded)) {
				t.Fatalf("decoded attribute is not valid JSON: %s\nraw=%s", decoded, out)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(decoded), &got); err != nil {
				t.Fatalf("unmarshal decoded: %v\n%s", err, decoded)
			}
			if got["value"] != tc.in {
				t.Fatalf("attribute round-trip mismatch: got %q want %q", got["value"], tc.in)
			}
		})
	}
}

// extractBetween pulls the substring between prefix and the following suffix in
// s, failing the test if either delimiter is absent.
func extractBetween(t *testing.T, s, prefix, suffix string) string {
	t.Helper()
	start := strings.Index(s, prefix)
	if start < 0 {
		t.Fatalf("missing prefix %q in %s", prefix, s)
	}
	start += len(prefix)
	end := strings.Index(s[start:], suffix)
	if end < 0 {
		t.Fatalf("missing suffix %q in %s", suffix, s[start:])
	}
	return s[start : start+end]
}
