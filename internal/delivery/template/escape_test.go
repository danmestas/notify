package template

import (
	"encoding/json"
	"strings"
	"testing"
)

// raw is the issue's payload (#364): every character Handlebars escapes.
const raw = `don't "quote" <tag> & more`

func noTranslate(k string) string { return k }

// TestRenderAs_EscapesByDestination: plain destinations get the value as
// sent, byte for byte; HTML gets it escaped with entities Telegram also
// accepts; Slack gets only its three reserved characters escaped.
func TestRenderAs_EscapesByDestination(t *testing.T) {
	data := map[string]any{"payload": map[string]any{"text": raw}}
	cases := []struct {
		name string
		esc  Escaper
		want string
	}{
		{"plain", Plain, raw},
		{"html", HTML, `don&#39;t &#34;quote&#34; &lt;tag&gt; &amp; more`},
		{"slack", Slack, `don't "quote" &lt;tag&gt; &amp; more`},
	}
	for _, c := range cases {
		if got := RenderAs(c.esc, "{{payload.text}}", data, noTranslate); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestRenderAs_TemplateMarkupIsNeverEscaped: the author's own markup is not
// data. An HTML template keeps its tags, and only the value is escaped.
func TestRenderAs_TemplateMarkupIsNeverEscaped(t *testing.T) {
	data := map[string]any{"payload": map[string]any{"name": "<script>"}}
	got := RenderAs(HTML, "<b>{{payload.name}}</b> & co", data, noTranslate)
	if want := "<b>&lt;script&gt;</b> & co"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestRenderAs_TripleBracesDoNotDoubleEscape: an author who already wrote
// {{{x}}} in an HTML template still gets the escaped value, not raw HTML.
// Triple braces opt out of Handlebars' escaping, which RenderAs no longer
// relies on; the destination escaping has already been applied.
func TestRenderAs_TripleBracesDoNotDoubleEscape(t *testing.T) {
	data := map[string]any{"payload": map[string]any{"x": "a&b"}}
	if got := RenderAs(HTML, "{{{payload.x}}}|{{payload.x}}", data, noTranslate); got != "a&amp;b|a&amp;b" {
		t.Fatalf("got %q", got)
	}
	if got := RenderAs(Plain, "{{{payload.x}}}|{{payload.x}}", data, noTranslate); got != "a&b|a&b" {
		t.Fatalf("plain got %q", got)
	}
}

// TestRenderAs_NestedAndTypedData: digest items, step outputs and each/if
// all see escaped strings; numbers and booleans are untouched.
func TestRenderAs_NestedAndTypedData(t *testing.T) {
	data := map[string]any{
		"digest": map[string]any{"count": 2, "events": []map[string]any{{"v": "it's"}, {"v": "a<b"}}},
		"steps":  map[string]map[string]any{"s1": {"out": "x&y", "ok": true}},
		"list":   []any{"o'k", 3.5},
	}
	tmpl := `{{digest.count}}:{{#each digest.events}}[{{v}}]{{/each}} {{steps.s1.out}} {{#if steps.s1.ok}}yes{{/if}} {{#each list}}{{this}},{{/each}}`
	if got, want := RenderAs(Plain, tmpl, data, noTranslate), `2:[it's][a<b] x&y yes o'k,3.5,`; got != want {
		t.Fatalf("plain: got %q want %q", got, want)
	}
	if got, want := RenderAs(HTML, tmpl, data, noTranslate), `2:[it&#39;s][a&lt;b] x&amp;y yes o&#39;k,3.5,`; got != want {
		t.Fatalf("html: got %q want %q", got, want)
	}
}

// TestRenderAs_TranslationsFollowTheDestination: a translation is inserted
// like a value, so an apostrophe in a French string survives a text email.
func TestRenderAs_TranslationsFollowTheDestination(t *testing.T) {
	tr := func(string) string { return "l'équipe <3" }
	if got := RenderAs(Plain, `{{t "team"}}`, nil, tr); got != "l'équipe <3" {
		t.Fatalf("plain: %q", got)
	}
	if got := RenderAs(HTML, `{{t "team"}}`, nil, tr); got != "l&#39;équipe &lt;3" {
		t.Fatalf("html: %q", got)
	}
}

// TestRenderAs_TrustedContentIsNotReEscaped: a layout's {{content}} is the
// already-rendered body, so it goes in as is while other values escape.
func TestRenderAs_TrustedContentIsNotReEscaped(t *testing.T) {
	data := map[string]any{"content": Trusted("<p>it&#39;s</p>"), "payload": map[string]any{"n": "<i>"}}
	if got, want := RenderAs(HTML, "<main>{{content}}</main>{{payload.n}}", data, noTranslate), "<main><p>it&#39;s</p></main>&lt;i&gt;"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestRenderAs_DoesNotMutateCallerData: the escaped copy is RenderAs's own;
// the handler reuses data for a second render (chat wire text).
func TestRenderAs_DoesNotMutateCallerData(t *testing.T) {
	inner := map[string]any{"x": "a&b"}
	data := map[string]any{"payload": inner}
	_ = RenderAs(HTML, "{{payload.x}}", data, noTranslate)
	if inner["x"] != "a&b" {
		t.Fatalf("caller data mutated: %v", inner["x"])
	}
	if got := RenderAs(Plain, "{{payload.x}}", data, noTranslate); got != "a&b" {
		t.Fatalf("second render got %q", got)
	}
}

// TestJSON_EscapesForAStringLiteral: a value placed inside a JSON string
// keeps the JSON valid and decodes back to itself, with <, > and & left
// alone rather than turned into \u003c escapes.
func TestJSON_EscapesForAStringLiteral(t *testing.T) {
	in := "say \"hi\"\\ <b> & \n\ttab \u2028"
	got := RenderAs(JSON, `{"text": "{{payload.x}}"}`, map[string]any{"payload": map[string]any{"x": in}}, noTranslate)
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("rendered %q is not JSON: %v", got, err)
	}
	if out.Text != in {
		t.Fatalf("round trip = %q, want %q", out.Text, in)
	}
	if strings.Contains(got, `\u003c`) {
		t.Fatalf("rendered %q escapes < as \\u003c", got)
	}
}
