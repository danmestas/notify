package webhooksout

import (
	"strings"
	"testing"
)

// TestNew_Client_BlocksPrivateIP proves the Consumer's HTTP client (used to
// POST delivery-status events to tenant-configured webhook endpoints, wired
// up by New) is SSRF-safe: a request to an RFC1918 address must be rejected
// at dial time, not silently sent by a plain &http.Client{}.
func TestNew_Client_BlocksPrivateIP(t *testing.T) {
	consumer := New(nil, nil)
	resp, err := consumer.client.Get("http://10.0.0.1/") //nolint:noctx
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("client.Get: expected SSRF guard error for RFC1918 10.0.0.1, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("client.Get error = %v, want it to contain %q", err, "ssrf guard:")
	}
}

// TestNew_Client_BlocksLoopback proves the same guard fires for a loopback
// target.
func TestNew_Client_BlocksLoopback(t *testing.T) {
	consumer := New(nil, nil)
	resp, err := consumer.client.Get("http://127.0.0.1:9999/") //nolint:noctx
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("client.Get: expected SSRF guard error for loopback 127.0.0.1, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("client.Get error = %v, want it to contain %q", err, "ssrf guard:")
	}
}
