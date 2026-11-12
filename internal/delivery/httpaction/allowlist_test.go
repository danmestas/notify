package httpaction

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The allowlist is process-wide, so every test here installs one and restores
// the empty default afterwards. Do not mark these t.Parallel().
func withAllowlist(t *testing.T, raw string) {
	t.Helper()
	if err := ConfigureAllowlist(raw); err != nil {
		t.Fatalf("ConfigureAllowlist(%q): %v", raw, err)
	}
	t.Cleanup(func() {
		if err := ConfigureAllowlist(""); err != nil {
			t.Fatalf("restoring empty allowlist: %v", err)
		}
	})
}

// TestAllowlist_DefaultBlocksLoopback is the baseline every existing deployment
// is in: no allowlist configured, so the guard must behave exactly as it did
// before the allowlist existed.
func TestAllowlist_DefaultBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := NewSSRFSafeClient().Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("reached a loopback server with no allowlist configured: the guard is off")
	}
}

// TestAllowlist_AllowsListedHost is the feature: the operator names the host and
// the request goes through, to a real server, on an address the guard blocks by
// default.
func TestAllowlist_AllowsListedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	defer srv.Close()

	withAllowlist(t, "127.0.0.1")

	resp, err := NewSSRFSafeClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("allowlisted host was still blocked: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "reached" {
		t.Errorf("body = %q, want %q", body, "reached")
	}
}

// TestAllowlist_OtherInternalHostsStillBlocked proves the exemption is scoped to
// the listed host and does not disable the guard globally.
func TestAllowlist_OtherInternalHostsStillBlocked(t *testing.T) {
	withAllowlist(t, "allowed.internal")

	resp, err := NewSSRFSafeClient().Get("http://127.0.0.1:9/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("an unlisted internal address was reachable while an allowlist was configured")
	}
}

// TestAllowlist_NoSuffixMatching is the whole reason wildcards are rejected: a
// listed parent domain must not admit an attacker-controlled subdomain.
func TestAllowlist_NoSuffixMatching(t *testing.T) {
	withAllowlist(t, "corp.example")

	if hostAllowed("evil.corp.example", "") {
		t.Error("evil.corp.example matched the entry corp.example: suffix matching is a subdomain-takeover hole")
	}
	if hostAllowed("corp.example.evil.com", "") {
		t.Error("corp.example.evil.com matched the entry corp.example: prefix matching")
	}
	if !hostAllowed("corp.example", "") {
		t.Error("the exact listed host did not match")
	}
}

// TestAllowlist_HostNormalisation covers the forms of the same host that must be
// treated as one, so an operator does not have to guess the spelling.
func TestAllowlist_HostNormalisation(t *testing.T) {
	withAllowlist(t, " Corp.Example. , ::1 , 10.0.0.5 ")

	for _, host := range []string{"corp.example", "CORP.EXAMPLE", "corp.example.", "Corp.Example"} {
		if !hostAllowed(host, "") {
			t.Errorf("hostAllowed(%q) = false, want true (case and root dot must not matter)", host)
		}
	}
	if !hostAllowed("0:0:0:0:0:0:0:1", "") {
		t.Error("the expanded form of ::1 did not match: IP entries must be compared canonically")
	}
	if !hostAllowed("10.0.0.5", "") {
		t.Error("a listed IPv4 literal did not match")
	}
}

// TestAllowlist_UnicodeCannotFoldOntoASCII pins the reason case folding is
// ASCII-only. Unicode folding maps U+212A KELVIN SIGN onto "k", which would let
// a non-ASCII host inherit an allowlisted ASCII host's exemption.
func TestAllowlist_UnicodeCannotFoldOntoASCII(t *testing.T) {
	withAllowlist(t, "bank.example")

	if hostAllowed("banK.example", "") {
		t.Error("a host containing U+212A matched the ASCII entry bank.example: case folding must be ASCII-only")
	}

	if err := ConfigureAllowlist("banK.example"); err == nil {
		t.Error("a non-ASCII entry was accepted; it must be rejected in favour of its punycode form")
	}
}

// TestAllowlist_EmptyAndBlankEntries keeps trailing commas and padding from
// being errors, since these come from an env var edited by hand.
func TestAllowlist_EmptyAndBlankEntries(t *testing.T) {
	withAllowlist(t, " , a.example ,, b.example , ")

	for _, host := range []string{"a.example", "b.example"} {
		if !hostAllowed(host, "") {
			t.Errorf("hostAllowed(%q) = false, want true", host)
		}
	}
	if hostAllowed("", "") {
		t.Error("the empty host matched: blank entries must not become an allowlist entry")
	}
}

// TestAllowlist_RejectsMisleadingEntries is the most important validation
// behaviour: an entry that looks like it should match a family of hosts must
// fail loudly at startup rather than silently match nothing, which would leave
// an operator believing they had access they do not have.
func TestAllowlist_RejectsMisleadingEntries(t *testing.T) {
	cases := map[string]string{
		"wildcard":       "*.corp.example",
		"bare wildcard":  "*",
		"cidr":           "10.0.0.0/8",
		"url":            "https://corp.example",
		"path":           "corp.example/api",
		"port zero":      "corp.example:0",
		"port too big":   "corp.example:65536",
		"port not num":   "corp.example:http",
		"empty port":     "corp.example:",
		"ipv6 no brkt":   "::1:3738x",
		"url with port":  "http://corp.example:8080",
		"embedded space": "corp example",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			err := ConfigureAllowlist(entry)
			t.Cleanup(func() { _ = ConfigureAllowlist("") })
			if err == nil {
				t.Fatalf("ConfigureAllowlist(%q) succeeded; it must reject an entry that cannot mean what it looks like", entry)
			}
			if !strings.Contains(err.Error(), "ssrf allowlist") {
				t.Errorf("error %q does not identify itself as an allowlist problem", err)
			}
		})
	}
}

// TestAllowlist_ReconfigureReplaces makes sure a later call does not merge into
// the previous allowlist -- an allowlist that only ever grows would be a hazard.
func TestAllowlist_ReconfigureReplaces(t *testing.T) {
	withAllowlist(t, "first.example")
	if !hostAllowed("first.example", "") {
		t.Fatal("first.example was not installed")
	}

	if err := ConfigureAllowlist("second.example"); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if hostAllowed("first.example", "") {
		t.Error("first.example survived a reconfigure: entries must be replaced, not accumulated")
	}
	if !hostAllowed("second.example", "") {
		t.Error("second.example was not installed")
	}
}

// TestAllowlist_RejectedEntryLeavesPreviousIntact: a bad config must not
// half-apply. Rejecting the whole value keeps the installed posture predictable.
func TestAllowlist_RejectedEntryLeavesPreviousIntact(t *testing.T) {
	withAllowlist(t, "good.example")

	if err := ConfigureAllowlist("also-good.example,*.bad.example"); err == nil {
		t.Fatal("a list containing a wildcard was accepted")
	}
	if hostAllowed("also-good.example", "") {
		t.Error("a valid entry from a rejected list was installed: the update must be all-or-nothing")
	}
	if !hostAllowed("good.example", "") {
		t.Error("the previously installed allowlist was clobbered by a rejected update")
	}
}

// loopbackServer listens on a kernel-chosen loopback port so the tests can
// build an entry from its real port.
func loopbackServer(t *testing.T, network, addr string) (*httptest.Server, string) {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return srv, port
}

func reaches(t *testing.T, url string) bool {
	t.Helper()
	resp, err := NewSSRFSafeClient().Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func TestAllowlist_HostPortAllowsOnlyThatPort(t *testing.T) {
	srv, port := loopbackServer(t, "tcp4", "127.0.0.1:0")
	_, other := loopbackServer(t, "tcp4", "127.0.0.1:0")

	withAllowlist(t, "127.0.0.1:"+port)
	if !reaches(t, srv.URL) {
		t.Fatal("127.0.0.1:<port> entry refused the listener on that port")
	}
	if reaches(t, "http://127.0.0.1:"+other) {
		t.Fatal("127.0.0.1:<port> entry admitted a different loopback port")
	}
}

func TestAllowlist_BareHostStillAllowsAnyPort(t *testing.T) {
	srvA, _ := loopbackServer(t, "tcp4", "127.0.0.1:0")
	srvB, _ := loopbackServer(t, "tcp4", "127.0.0.1:0")
	withAllowlist(t, "127.0.0.1")
	if !reaches(t, srvA.URL) || !reaches(t, srvB.URL) {
		t.Fatal("bare host entry must allow every port")
	}
}

func TestAllowlist_IPv6HostPort(t *testing.T) {
	srv, port := loopbackServer(t, "tcp6", "[::1]:0")
	_, other := loopbackServer(t, "tcp6", "[::1]:0")
	withAllowlist(t, "[::1]:"+port)
	if !reaches(t, srv.URL) {
		t.Fatal("[::1]:<port> entry refused the listener on that port")
	}
	if reaches(t, "http://[::1]:"+other) {
		t.Fatal("[::1]:<port> entry admitted a different port")
	}
}

func TestAllowlist_HostnameWithPort(t *testing.T) {
	withAllowlist(t, "Corp.Example:8080")
	if !hostAllowed("corp.example", "8080") {
		t.Error("hostname:port entry should match that host and port")
	}
	if hostAllowed("corp.example", "9090") || hostAllowed("corp.example", "") {
		t.Error("hostname:port entry matched another port")
	}
	if hostAllowed("evil.corp.example", "8080") {
		t.Error("hostname:port entry matched a different host")
	}
}

func TestAllowlist_PortErrorIsHelpful(t *testing.T) {
	err := ConfigureAllowlist("127.0.0.1:99999")
	t.Cleanup(func() { _ = ConfigureAllowlist("") })
	if err == nil || !strings.Contains(err.Error(), "1 to 65535") {
		t.Fatalf("want a port-range error, got %v", err)
	}
}
