package mastodon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.ChatProvider = (*Client)(nil)

type captured struct {
	method string
	path   string
	header http.Header
	body   string
}

func stubServer(t *testing.T, status int, respBody string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.path = r.URL.Path
		got.header = r.Header.Clone()
		got.body = string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestClient_Send_Wire pins the exact Mastodon wire: POST {base}/api/v1/statuses,
// Authorization: Bearer <token>, form-encoded status + visibility.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"110000000000000001"}`)

	c := &Client{BaseURL: srv.URL, AccessToken: "test-token-123"}
	res, err := c.Send(context.Background(), providers.ChatMessage{Content: "Hello from notify"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "110000000000000001" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "110000000000000001")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/api/v1/statuses" {
		t.Errorf("path = %q, want /api/v1/statuses", got.path)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer test-token-123" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer test-token-123")
	}
	vals, err := url.ParseQuery(got.body)
	if err != nil {
		t.Fatalf("body not form-encoded: %v (%q)", err, got.body)
	}
	if vals.Get("status") != "Hello from notify" {
		t.Errorf("status = %q, want %q", vals.Get("status"), "Hello from notify")
	}
	if vals.Get("visibility") != "public" {
		t.Errorf("visibility = %q, want default public", vals.Get("visibility"))
	}
}

// TestClient_Send_ConfiguredVisibility: a configured Visibility overrides the
// default "public".
func TestClient_Send_ConfiguredVisibility(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"1"}`)

	c := &Client{BaseURL: srv.URL, AccessToken: "t", Visibility: "unlisted"}
	if _, err := c.Send(context.Background(), providers.ChatMessage{Content: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	vals, _ := url.ParseQuery(got.body)
	if vals.Get("visibility") != "unlisted" {
		t.Errorf("visibility = %q, want unlisted", vals.Get("visibility"))
	}
}

// TestClient_Send_MentionPrepend: a non-empty Address is a Mastodon @handle
// and is prepended to the status text ("@handle status"). With Visibility
// "direct" this addresses a DM to that handle.
func TestClient_Send_MentionPrepend(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"1"}`)

	c := &Client{BaseURL: srv.URL, AccessToken: "t", Visibility: "direct"}
	if _, err := c.Send(context.Background(), providers.ChatMessage{
		Address: "@friend@example.social",
		Content: "hi there",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	vals, _ := url.ParseQuery(got.body)
	if want := "@friend@example.social hi there"; vals.Get("status") != want {
		t.Errorf("status = %q, want %q", vals.Get("status"), want)
	}
	if vals.Get("visibility") != "direct" {
		t.Errorf("visibility = %q, want direct", vals.Get("visibility"))
	}
}

// TestClient_Send_NoIdempotencyKeyHeader: Send must NOT set an
// Idempotency-Key header derived from Address+Content. ChatMessage carries no
// field that identifies "this notification" independent of delivery
// bookkeeping, so two distinct notifications that render identically for the
// same destination (a recurring alert firing twice, a monitor re-triggering)
// would otherwise hash to the same key; Mastodon honors Idempotency-Key for
// about an hour and would silently swallow the second post as a no-op. The
// delivery engine's own retry bookkeeping already bounds duplicate sends, so
// no key is sent until a genuine per-delivery unique id is plumbed in.
func TestClient_Send_NoIdempotencyKeyHeader(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"1"}`)

	c := &Client{BaseURL: srv.URL, AccessToken: "t"}
	if _, err := c.Send(context.Background(), providers.ChatMessage{Content: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if key := got.header.Get("Idempotency-Key"); key != "" {
		t.Errorf("Idempotency-Key header = %q, want unset", key)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error that
// contains the status code and never the access token.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	const token = "super-secret-token-xyz"
	srv, _ := stubServer(t, http.StatusUnauthorized, `{"error":"The access token is invalid"}`)

	c := &Client{BaseURL: srv.URL, AccessToken: token}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on 401: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("401 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 401") {
		t.Errorf("error %q missing status code", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks access token: %q", err.Error())
	}
}

// TestClient_Send_5xxIsTransient: a 5xx is wrapped as TransientError and never
// leaks the access token.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	const token = "super-secret-token-xyz"
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)

	c := &Client{BaseURL: srv.URL, AccessToken: token}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on 503: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("503 classified permanent, want transient: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks access token: %q", err.Error())
	}
}

// TestClient_Send_EmptyTokenIsPermanent: no access token fails closed.
func TestClient_Send_EmptyTokenIsPermanent(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:0"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send with empty token: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty token classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_EmptyBaseURLIsPermanent: no base URL fails closed.
func TestClient_Send_EmptyBaseURLIsPermanent(t *testing.T) {
	c := &Client{AccessToken: "t"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send with empty base url: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty base url classified transient, want permanent: %v", err)
	}
}
