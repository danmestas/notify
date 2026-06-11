package ntfy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.PushProvider = (*Client)(nil)

// captured is one request observed by the stub ntfy server.
type captured struct {
	method string
	path   string
	header http.Header
	body   string
}

// stubServer returns an httptest server answering status/respBody and a
// pointer to the last captured request.
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

// TestClient_Send_Wire pins the exact ntfy wire (verified against a live ntfy
// 2.x server): POST {base}/{topic}, Authorization Bearer, X-Title, X-Priority,
// X-Tags comma-joined, X-Markdown, message text as the body.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"ntfy-msg-1","topic":"agent-alerts"}`)

	c := &Client{BaseURL: srv.URL, Token: "tk_secret"}
	res, err := c.Send(context.Background(), providers.PushMessage{
		Topic:    "agent-alerts",
		Title:    "Build failed",
		Body:     "notify CI is red",
		Priority: 4,
		Tags:     []string{"warning", "skull"},
		Markdown: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "ntfy-msg-1" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "ntfy-msg-1")
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/agent-alerts" {
		t.Errorf("path = %q, want /agent-alerts", got.path)
	}
	if h := got.header.Get("Authorization"); h != "Bearer tk_secret" {
		t.Errorf("Authorization = %q, want %q", h, "Bearer tk_secret")
	}
	if h := got.header.Get("X-Title"); h != "Build failed" {
		t.Errorf("X-Title = %q, want %q", h, "Build failed")
	}
	if h := got.header.Get("X-Priority"); h != "4" {
		t.Errorf("X-Priority = %q, want %q", h, "4")
	}
	if h := got.header.Get("X-Tags"); h != "warning,skull" {
		t.Errorf("X-Tags = %q, want %q", h, "warning,skull")
	}
	if h := got.header.Get("X-Markdown"); h != "yes" {
		t.Errorf("X-Markdown = %q, want %q", h, "yes")
	}
	if got.body != "notify CI is red" {
		t.Errorf("body = %q, want %q", got.body, "notify CI is red")
	}
}

// TestClient_Send_OmitsOptionalHeaders: no token/title/priority/tags/markdown
// means the corresponding headers are absent, not empty-valued.
func TestClient_Send_OmitsOptionalHeaders(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)

	c := &Client{BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, h := range []string{"Authorization", "X-Title", "X-Priority", "X-Tags", "X-Markdown"} {
		if _, ok := got.header[h]; ok {
			t.Errorf("header %s present, want absent (value %q)", h, got.header.Get(h))
		}
	}
}

// TestClient_Send_EmptyTopicFailsClosed: an empty topic is a permanent (plain)
// error before any request is made — it must not POST to the base URL root.
func TestClient_Send_EmptyTopicFailsClosed(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)

	c := &Client{BaseURL: srv.URL, Token: "tk"}
	_, err := c.Send(context.Background(), providers.PushMessage{Body: "b"})
	if err == nil {
		t.Fatal("Send with empty topic: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty topic classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "empty topic") {
		t.Errorf("error %q missing empty-topic cause", err)
	}
	if got.method != "" {
		t.Errorf("server received a %s request, want none", got.method)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error
// carrying the status and response body.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusForbidden, `{"error":"forbidden"}`)

	c := &Client{BaseURL: srv.URL, Token: "bad"}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send on 403: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("403 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 403") {
		t.Errorf("error %q missing status code", err)
	}
}

// TestClient_Send_5xxIsTransient: a 5xx is wrapped as TransientError.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)

	c := &Client{BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send on 503: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("503 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_TimeoutIsTransient: a client-side timeout is transient.
func TestClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
