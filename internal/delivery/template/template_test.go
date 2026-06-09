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
		{
			name: "simple map path",
			tmpl: "{{subscriber.firstName}}",
			data: map[string]any{"subscriber": map[string]any{"firstName": "Ada"}},
			want: "Ada",
		},
		{
			name: "spaces around path and number leaf",
			tmpl: "{{ payload.itemCount }}",
			data: map[string]any{"payload": map[string]any{"itemCount": 3}},
			want: "3",
		},
		{
			name: "missing path yields empty",
			tmpl: "{{payload.nope}}",
			data: map[string]any{"payload": map[string]any{"itemCount": 3}},
			want: "",
		},
		{
			name: "struct resolves via json tag",
			tmpl: "{{subscriber.firstName}}",
			data: map[string]any{"subscriber": subscriber{FirstName: "Ada"}},
			want: "Ada",
		},
		{
			name: "unterminated token copied verbatim",
			tmpl: "{{payload.x",
			data: map[string]any{"payload": map[string]any{"x": "v"}},
			want: "{{payload.x",
		},
		{
			name: "mixed literal and token",
			tmpl: "Hi {{subscriber.firstName}}!",
			data: map[string]any{"subscriber": map[string]any{"firstName": "Ada"}},
			want: "Hi Ada!",
		},
		{
			name: "non-leaf path points at a map yields empty",
			tmpl: "{{subscriber}}",
			data: map[string]any{"subscriber": map[string]any{"firstName": "Ada"}},
			want: "",
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Render(tt.tmpl, tt.data); got != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}
