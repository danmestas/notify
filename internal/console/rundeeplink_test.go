package console

import (
	"strings"
	"testing"
)

// TestRunURL_ConsolePathAndEscaping pins the wire contract for dagnats run
// deep-links.
//
// Two independent things have to hold, and a previous hand-rolled call site in
// integrations.go got both wrong: the path prefix must be "/console/runs/"
// (dagnats does not serve a bare "/runs/"), and the id must be PathEscaped.
// The escaping is not cosmetic — run ids are DB-stored and the lookup that
// produces them is unscoped, so an id is not guaranteed to stay an opaque ULID.
func TestRunURL_ConsolePathAndEscaping(t *testing.T) {
	c := &Console{deps: Deps{DagnatsConsoleURL: "https://engine.example"}}

	for _, tc := range []struct {
		name, runID, want string
	}{
		{
			name:  "plain ulid",
			runID: "01KXW4DN0PMCBEE1A77PXEA2ZR",
			want:  "https://engine.example/console/runs/01KXW4DN0PMCBEE1A77PXEA2ZR",
		},
		{
			name:  "traversal attempt is escaped, not honoured",
			runID: "../../etc/passwd",
			want:  "https://engine.example/console/runs/..%2F..%2Fetc%2Fpasswd",
		},
		{
			name:  "space is escaped",
			runID: "a b",
			want:  "https://engine.example/console/runs/a%20b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := c.runURL(tc.runID)
			if got != tc.want {
				t.Fatalf("runURL(%q) = %q, want %q", tc.runID, got, tc.want)
			}
			if strings.Contains(got, "/console/runs/") == false {
				t.Fatalf("missing /console/runs/ prefix: %q", got)
			}
		})
	}
}

// TestRunURL_EmptyWhenUnconfigured keeps an unset base (or empty id) from
// producing a bare relative path, which templates would happily render as a
// dead same-origin link instead of falling back to plain text.
func TestRunURL_EmptyWhenUnconfigured(t *testing.T) {
	unset := &Console{deps: Deps{}}
	if got := unset.runURL("01KXW4DN0PMCBEE1A77PXEA2ZR"); got != "" {
		t.Fatalf("unset base: runURL = %q, want empty", got)
	}
	set := &Console{deps: Deps{DagnatsConsoleURL: "https://engine.example"}}
	if got := set.runURL(""); got != "" {
		t.Fatalf("empty id: runURL = %q, want empty", got)
	}
}
