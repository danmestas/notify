package template

import "testing"

type subscriber struct {
	FirstName string `json:"firstName"`
}

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		data map[string]any
		want string
	}{
		// Path interpolation: the behavior delivery handlers rely on.
		{
			name: "simple map path",
			tmpl: "Hi {{subscriber.firstName}}",
			data: map[string]any{"subscriber": map[string]any{"firstName": "Ada"}},
			want: "Hi Ada",
		},
		{
			name: "number leaf",
			tmpl: "You have {{payload.itemCount}} items",
			data: map[string]any{"payload": map[string]any{"itemCount": 3}},
			want: "You have 3 items",
		},
		{
			name: "missing path yields empty",
			tmpl: "{{payload.nope}}",
			data: map[string]any{"payload": map[string]any{"itemCount": 3}},
			want: "",
		},
		{
			name: "digest count",
			tmpl: "You have {{digest.count}} new items",
			data: map[string]any{"digest": map[string]any{"count": 3}},
			want: "You have 3 new items",
		},
		{
			name: "struct resolves via json tag",
			tmpl: "{{subscriber.firstName}}",
			data: map[string]any{"subscriber": subscriber{FirstName: "Ada"}},
			want: "Ada",
		},
		{
			name: "bool leaf",
			tmpl: "{{flags.enabled}}",
			data: map[string]any{"flags": map[string]any{"enabled": true}},
			want: "true",
		},
		{
			name: "no token literal verbatim",
			tmpl: "plain text",
			data: nil,
			want: "plain text",
		},

		// Full Handlebars features now available.
		{
			name: "if truthy branch",
			tmpl: "{{#if x}}A{{else}}B{{/if}}",
			data: map[string]any{"x": true},
			want: "A",
		},
		{
			name: "if falsy branch",
			tmpl: "{{#if x}}A{{else}}B{{/if}}",
			data: map[string]any{"x": false},
			want: "B",
		},
		{
			name: "each over slice",
			tmpl: "{{#each items}}{{this}},{{/each}}",
			data: map[string]any{"items": []string{"a", "b", "c"}},
			want: "a,b,c,",
		},
		{
			name: "with block",
			tmpl: "{{#with subscriber}}{{firstName}}{{/with}}",
			data: map[string]any{"subscriber": map[string]any{"firstName": "Ada"}},
			want: "Ada",
		},

		// Degrade behavior: a malformed template renders as its literal source.
		{
			name: "unterminated token degrades to source",
			tmpl: "{{oops",
			data: map[string]any{"oops": "v"},
			want: "{{oops",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Render(tt.tmpl, tt.data); got != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}

func TestRenderAs_Translate(t *testing.T) {
	translate := func(key string) string {
		if key == "greeting" {
			return "Hola"
		}
		return key
	}
	data := map[string]any{"subscriber": map[string]any{"firstName": "Ada"}}
	if got := RenderAs(Plain, `{{t "greeting"}} {{subscriber.firstName}}`, data, translate); got != "Hola Ada" {
		t.Fatalf("RenderAs with {{t}}: got %q want %q", got, "Hola Ada")
	}
}
