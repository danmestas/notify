package console

// Methodology: BasePath unit tests for the two emitters not reached by the
// notify-level browse test -- the stream-sources ingest snippet origin and the
// page-header tile link -- run against a Console with a chosen BasePath.

import (
	"strings"
	"testing"
)

func TestIngestBase_IncludesBasePath(t *testing.T) {
	cases := []struct {
		name string
		deps Deps
		want string
	}{
		{"public url", Deps{PublicBaseURL: "https://n.example.com", BasePath: "/notify"}, "https://n.example.com/notify"},
		{"http addr", Deps{HTTPAddr: "http://127.0.0.1:3000", BasePath: "/notify"}, "http://127.0.0.1:3000/notify"},
		{"http addr no prefix", Deps{HTTPAddr: "http://127.0.0.1:3000"}, "http://127.0.0.1:3000"},
		{"placeholder", Deps{BasePath: "/notify"}, "$BASE"},
	}
	for _, tc := range cases {
		c := &Console{deps: tc.deps}
		if got := c.ingestBase(); got != tc.want {
			t.Errorf("%s: ingestBase() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPageHeader_TileHrefHonoursBasePath(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.BasePath = "/notify"
	cases := []struct{ href, want string }{
		{"#", `href="#"`},
		{"https://example.com/x", `href="https://example.com/x"`},
		{"/console/workflows", `href="/notify/console/workflows"`},
	}
	for _, cs := range cases {
		out, err := tc.c.renderPartial("page-header", pageHeader{
			Title: "T",
			Tiles: []tile{{Label: "L", Count: 1, Href: cs.href}},
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(string(out), cs.want) {
			t.Errorf("tile Href %q: output lacks %s:\n%s", cs.href, cs.want, out)
		}
	}
}
