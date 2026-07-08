package webhook

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// TestPost_NilClient_BlocksPrivateIP proves that post()'s nil-HTTPClient
// fallback (used whenever a caller does not set EmailClient/PushClient/
// ChatClient.HTTPClient) is the SSRF-safe client, not a plain
// &http.Client{}: a tenant-supplied endpoint pointing at an RFC1918 address
// must be rejected at dial time.
func TestPost_NilClient_BlocksPrivateIP(t *testing.T) {
	_, err := post(context.Background(), nil, "http://10.0.0.1/hook", "", struct{}{}, "test-webhook")
	if err == nil {
		t.Fatal("post: expected SSRF guard error for RFC1918 10.0.0.1, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("post error = %v, want it to contain %q", err, "ssrf guard:")
	}
	// A dial-time rejection is a transport fault, which post() classifies as
	// retry-safe (providers.TransientError).
	if !providers.IsTransient(err) {
		t.Errorf("SSRF-blocked post classified permanent, want transient: %v", err)
	}
}

// TestChatClient_Send_NilClient_BlocksLoopback proves the same guard fires
// through the public ChatClient.Send path when no HTTPClient is configured
// and Address (tenant/subscriber-supplied) targets loopback.
func TestChatClient_Send_NilClient_BlocksLoopback(t *testing.T) {
	c := &ChatClient{URL: "http://127.0.0.1:9999/hook"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "hi"})
	if err == nil {
		t.Fatal("Send: expected SSRF guard error for loopback URL, got nil")
	}
	if !strings.Contains(err.Error(), "ssrf guard:") {
		t.Errorf("Send error = %v, want it to contain %q", err, "ssrf guard:")
	}
}
