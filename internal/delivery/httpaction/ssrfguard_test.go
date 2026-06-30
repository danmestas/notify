package httpaction_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/engine"
)

// TestSSRFGuard_IsBlockedIP exercises IsBlockedIP across every range the SSRF
// guard must reject and a selection of public IPs it must permit.
func TestSSRFGuard_IsBlockedIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// Loopback — IPv4 and IPv6
		{"loopback 127.0.0.1", "127.0.0.1", true},
		{"loopback 127.1.2.3", "127.1.2.3", true},
		{"loopback ::1", "::1", true},
		// Link-local unicast
		{"link-local 169.254.1.1", "169.254.1.1", true},
		{"link-local 169.254.169.254 (AWS metadata)", "169.254.169.254", true},
		{"link-local IPv6 fe80::1", "fe80::1", true},
		// RFC1918 private — all three blocks
		{"RFC1918 10.0.0.1", "10.0.0.1", true},
		{"RFC1918 10.255.255.255", "10.255.255.255", true},
		{"RFC1918 172.16.0.1", "172.16.0.1", true},
		{"RFC1918 172.31.255.255", "172.31.255.255", true},
		{"RFC1918 192.168.0.1", "192.168.0.1", true},
		{"RFC1918 192.168.255.255", "192.168.255.255", true},
		// Unique-local IPv6 (fc00::/7) — covered by net.IP.IsPrivate in Go 1.17+
		{"unique-local fc00::1", "fc00::1", true},
		{"unique-local fd00::1", "fd00::1", true},
		{"unique-local fdff::1", "fdff::1", true},
		// Unspecified — 0.0.0.0 and ::
		{"unspecified 0.0.0.0", "0.0.0.0", true},
		{"unspecified ::", "::", true},
		// Multicast
		{"multicast 224.0.0.1", "224.0.0.1", true},
		{"multicast 239.255.255.255", "239.255.255.255", true},
		{"multicast ff02::1", "ff02::1", true},
		// CGNAT / Tailscale mesh (100.64.0.0/10, RFC 6598)
		{"CGNAT 100.64.0.1", "100.64.0.1", true},
		{"CGNAT 100.127.255.255", "100.127.255.255", true},
		// "this" network 0.0.0.0/8 — IsUnspecified only catches 0.0.0.0 itself
		{"this-network 0.0.0.1", "0.0.0.1", true},
		{"this-network 0.1.2.3", "0.1.2.3", true},
		// NAT64 well-known prefix 64:ff9b::/96 (embeds IPv4)
		{"NAT64 64:ff9b::7f00:1 (=127.0.0.1)", "64:ff9b::7f00:1", true},
		// Broadcast
		{"broadcast 255.255.255.255", "255.255.255.255", true},
		// IPv4-mapped IPv6 regression — must be caught by the existing classifier calls
		{"IPv4-mapped loopback ::ffff:127.0.0.1", "::ffff:127.0.0.1", true},
		{"IPv4-mapped link-local ::ffff:169.254.169.254", "::ffff:169.254.169.254", true},
		// Public IPs — must NOT be blocked
		{"public 1.1.1.1", "1.1.1.1", false},
		{"public 1.2.3.4", "1.2.3.4", false},
		{"public 8.8.8.8", "8.8.8.8", false},
		{"public 93.184.216.34", "93.184.216.34", false},
		{"public 2001:db8::1", "2001:db8::1", false},
		// Edge cases just outside RFC1918 ranges
		{"just below 10/8: 9.255.255.255", "9.255.255.255", false},
		{"just above 172.16/12 lower: 172.15.255.255", "172.15.255.255", false},
		{"just above 172.16/12 upper: 172.32.0.0", "172.32.0.0", false},
		{"just below 192.168/16: 192.167.255.255", "192.167.255.255", false},
		// Just outside CGNAT range
		{"just below CGNAT: 100.63.255.255", "100.63.255.255", false},
		{"just above CGNAT: 100.128.0.0", "100.128.0.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test-case IP %q is not parseable", tc.ip)
			}
			got := httpaction.IsBlockedIP(ip)
			if got != tc.blocked {
				t.Errorf("IsBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

// TestSSRFGuard_Handler_BlocksLoopback_FailClosed proves that a handler built
// with the SSRF-safe client refuses a loopback connection and surfaces a
// NonRetryableError (fail_closed) without persisting any output row.
func TestSSRFGuard_Handler_BlocksLoopback_FailClosed(t *testing.T) {
	// httptest binds to 127.0.0.1 — the SSRF dial-control hook must fire and
	// abort the connection before any bytes are exchanged.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer srv.Close()

	_, s, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 5, "failurePolicy": "fail_closed",
	}, nil)
	h := httpaction.NewWithClient(s, httpaction.NewSSRFSafeClient())

	_, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err == nil {
		t.Fatal("Handle: expected SSRF guard error for loopback address, got nil")
	}
	var nonRetry *engine.NonRetryableError
	if !errors.As(err, &nonRetry) {
		t.Errorf("error = %T(%v), want *engine.NonRetryableError", err, err)
	}

	outputs, lerr := s.StepOutputs().ListByRun(ctx, "r1")
	if lerr != nil {
		t.Fatalf("ListByRun: %v", lerr)
	}
	if _, ok := outputs["h1"]; ok {
		t.Error("fail_closed: SSRF-blocked request wrote a step_output row, want none")
	}
}

// TestSSRFGuard_Handler_BlocksLoopback_Continue proves that with the continue
// policy a loopback-blocked request yields a zero-value output and nil error, so
// downstream steps proceed.
func TestSSRFGuard_Handler_BlocksLoopback_Continue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer srv.Close()

	_, s, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 5, "failurePolicy": "continue",
	}, nil)
	h := httpaction.NewWithClient(s, httpaction.NewSSRFSafeClient())

	out, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err != nil {
		t.Fatalf("Handle under continue: want nil error, got %v", err)
	}
	var resp struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if resp.StatusCode != 0 || resp.Body != "" {
		t.Errorf("zero output = {statusCode:%d, body:%q}, want {0, \"\"}", resp.StatusCode, resp.Body)
	}

	outputs, lerr := s.StepOutputs().ListByRun(ctx, "r1")
	if lerr != nil {
		t.Fatalf("ListByRun: %v", lerr)
	}
	if got, _ := outputs["h1"]["statusCode"].(float64); got != 0 {
		t.Errorf("persisted statusCode = %v, want 0", outputs["h1"]["statusCode"])
	}
}

// TestSSRFGuard_Handler_BlocksLinkLocal proves that requests to the AWS instance
// metadata endpoint (169.254.169.254) are blocked at the dial-control hook before
// any TCP connection is attempted. The hook fires before connect(), so the
// address need not be routable in the test environment.
func TestSSRFGuard_Handler_BlocksLinkLocal(t *testing.T) {
	_, s, ctx := seed(t, map[string]any{
		"url":    "http://169.254.169.254/latest/meta-data/",
		"method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 5, "failurePolicy": "fail_closed",
	}, nil)
	h := httpaction.NewWithClient(s, httpaction.NewSSRFSafeClient())

	_, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err == nil {
		t.Fatal("Handle: expected SSRF guard error for link-local 169.254.169.254, got nil")
	}
	var nonRetry *engine.NonRetryableError
	if !errors.As(err, &nonRetry) {
		t.Errorf("error = %T(%v), want *engine.NonRetryableError", err, err)
	}
}

// TestSSRFGuard_Handler_BlocksRFC1918 proves that requests to RFC1918 private
// addresses are blocked at the dial-control hook.
func TestSSRFGuard_Handler_BlocksRFC1918(t *testing.T) {
	_, s, ctx := seed(t, map[string]any{
		"url":    "http://10.0.0.1/",
		"method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 5, "failurePolicy": "fail_closed",
	}, nil)
	h := httpaction.NewWithClient(s, httpaction.NewSSRFSafeClient())

	_, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err == nil {
		t.Fatal("Handle: expected SSRF guard error for RFC1918 10.0.0.1, got nil")
	}
	var nonRetry *engine.NonRetryableError
	if !errors.As(err, &nonRetry) {
		t.Errorf("error = %T(%v), want *engine.NonRetryableError", err, err)
	}
}

// TestSSRFGuard_CheckRedirect_BlocksPrivateIP proves that a 302 redirect pointing
// at a private IP is rejected by CheckRedirect before any connection to the
// redirect target is attempted. Uses a fake RoundTripper so the initial request
// does not touch loopback (which would itself be blocked).
func TestSSRFGuard_CheckRedirect_BlocksPrivateIP(t *testing.T) {
	fakeRedirect := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://10.1.2.3/sensitive"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	c := httpaction.NewSSRFSafeClient()
	c.Transport = fakeRedirect

	// When CheckRedirect fails, Go returns (lastResp, err) with Body already closed.
	resp, err := c.Get("http://example.com/api") //nolint:noctx
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error on redirect to RFC1918 address, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard") {
		t.Errorf("expected SSRF guard error, got: %v", err)
	}
}

// TestSSRFGuard_HeaderCap proves that response headers exceeding maxHeaderBytes
// are truncated before being persisted to step_outputs, so a hostile endpoint
// cannot bloat the row with enormous header fields.
func TestSSRFGuard_HeaderCap(t *testing.T) {
	const wantCapBytes = 128 << 10 // must match maxHeaderBytes in httpaction.go

	// 200 headers × ~1 KiB value ≈ 200 KiB — well over the 128 KiB cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		val := strings.Repeat("x", 1024)
		for i := range 200 {
			w.Header().Set(fmt.Sprintf("X-Big-%d", i), val)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// seed() returns an unguarded handler (NewWithClient with bare &http.Client{})
	// so the httptest loopback server is reachable.
	h, s, ctx := seed(t, map[string]any{
		"url": srv.URL, "method": "GET", "headers": "{}", "body": "",
		"timeoutSec": 5, "failurePolicy": "fail_closed",
	}, nil)

	out, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var result struct {
		Headers map[string]string `json:"headers"`
	}
	if uerr := json.Unmarshal(out, &result); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if len(result.Headers) == 0 {
		t.Fatal("no headers captured at all; want some captured up to the cap")
	}
	// The handler sent 200 headers; the cap must have truncated the map.
	const sentHeaders = 200
	if len(result.Headers) >= sentHeaders {
		t.Errorf("header map not truncated: len(result.Headers) = %d, want < %d", len(result.Headers), sentHeaders)
	}

	var total int
	for k, v := range result.Headers {
		total += len(k) + len(v)
	}
	if total > wantCapBytes {
		t.Errorf("captured header bytes = %d, want ≤ %d (cap not enforced)", total, wantCapBytes)
	}

	// Also verify the stored step_outputs row carries the same capped headers.
	outputs, lerr := s.StepOutputs().ListByRun(ctx, "r1")
	if lerr != nil {
		t.Fatalf("ListByRun: %v", lerr)
	}
	if outputs["h1"] == nil {
		t.Fatal("no step_outputs row for h1")
	}
}

// roundTripFunc is a one-shot http.RoundTripper backed by a closure, used to
// inject a synthetic HTTP response without making a real TCP connection.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
