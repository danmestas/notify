package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.ChatProvider = (*Client)(nil)

// captured is one request observed by the stub Discord webhook server.
type captured struct {
	method string
	path   string
	query  string
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
		got.query = r.URL.RawQuery
		got.header = r.Header.Clone()
		got.body = string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestClient_Send_Wire pins the exact Discord webhook wire: POST
// {webhookURL}?wait=true with JSON {"content": <text>}, Content-Type
// application/json, and no auth header.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"disc-msg-1","timestamp":"2026-01-01T00:00:00Z"}`)

	c := &Client{WebhookURL: srv.URL + "/api/webhooks/123/abc"}
	res, err := c.Send(context.Background(), providers.ChatMessage{Content: "notify CI is red"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "disc-msg-1" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "disc-msg-1")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/api/webhooks/123/abc" {
		t.Errorf("path = %q, want /api/webhooks/123/abc", got.path)
	}
	if got.query != "wait=true" {
		t.Errorf("query = %q, want wait=true", got.query)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got.header.Get("Authorization") != "" {
		t.Errorf("Authorization header set, want absent: %q", got.header.Get("Authorization"))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, got.body)
	}
	if payload["content"] != "notify CI is red" {
		t.Errorf("body content = %v, want %q", payload["content"], "notify CI is red")
	}
	if len(payload) != 1 {
		t.Errorf("body has %d keys, want only {content}: %v", len(payload), payload)
	}
}

// TestClient_Send_AddressOverridesWebhook: a non-empty ChatMessage.Address is
// used as the destination instead of the configured WebhookURL.
func TestClient_Send_AddressOverridesWebhook(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"x"}`)

	c := &Client{WebhookURL: "https://discord.com/api/webhooks/should/not/be/used"}
	if _, err := c.Send(context.Background(), providers.ChatMessage{
		Address: srv.URL + "/api/webhooks/override/tok",
		Content: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.path != "/api/webhooks/override/tok" {
		t.Errorf("path = %q, want override path", got.path)
	}
}

// TestClient_Send_EmptyWebhookIsPermanent: no webhook url and no address fails
// closed with a permanent error rather than POSTing nowhere.
func TestClient_Send_EmptyWebhookIsPermanent(t *testing.T) {
	c := &Client{}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send with empty webhook: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty webhook classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusNotFound, `{"message":"Unknown Webhook"}`)

	c := &Client{WebhookURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on 404: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("404 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Errorf("error %q missing status code", err)
	}
}

// TestClient_Send_5xxIsTransient: a 5xx is wrapped as TransientError.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadGateway, ``)

	c := &Client{WebhookURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on 502: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("502 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_TransportErrorHidesWebhookToken: a transport fault returns a
// *url.Error embedding the request URL, which carries the webhook token in the
// path (/api/webhooks/{id}/{token}). The returned (transient) error must NOT
// leak the token. The *url.Error layer is unwrapped before wrapping.
func TestClient_Send_TransportErrorHidesWebhookToken(t *testing.T) {
	const token = "supersecretwebhooktoken0123456789abcdef"
	c := &Client{
		WebhookURL: "http://127.0.0.1:0/api/webhooks/123/" + token,
		HTTPClient: &http.Client{Timeout: 1 * time.Second},
	}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "hi"})
	if err == nil {
		t.Fatal("Send on transport fault: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("transport fault classified permanent, want transient: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks webhook token: %q", err.Error())
	}
}

// TestClient_Send_TimeoutIsTransient: a client-side timeout is transient.
func TestClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &Client{WebhookURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
