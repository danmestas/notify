package slack

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

// captured is one request observed by the stub Slack server.
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

// TestClient_Send_Wire pins the exact Slack wire (mirrors the reference
// slack.provider): POST {base}/chat.postMessage with Authorization: Bearer
// <botToken>, Content-Type application/json, and a {channel,text} body. On
// ok=true the provider message id is the response JSON "ts".
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"ok":true,"ts":"1503435956.000247"}`)

	c := &Client{BotToken: "xoxb-secret", BaseURL: srv.URL}
	res, err := c.Send(context.Background(), providers.ChatMessage{
		Address: "C12345",
		Content: "notify CI is red",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "1503435956.000247" {
		t.Errorf("ProviderMessageID = %q, want 1503435956.000247", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/chat.postMessage" {
		t.Errorf("path = %q, want /chat.postMessage", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer xoxb-secret" {
		t.Errorf("Authorization = %q, want Bearer xoxb-secret", auth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("parse body json: %v", err)
	}
	if payload["channel"] != "C12345" {
		t.Errorf("channel = %q, want C12345", payload["channel"])
	}
	if payload["text"] != "notify CI is red" {
		t.Errorf("text = %q, want notify CI is red", payload["text"])
	}
}

// TestClient_Send_OKFalsePermanent: HTTP 200 with ok=false and a non-transient
// error code is a permanent error (no retry).
func TestClient_Send_OKFalsePermanent(t *testing.T) {
	for _, code := range []string{"invalid_auth", "channel_not_found"} {
		srv, _ := stubServer(t, http.StatusOK, `{"ok":false,"error":"`+code+`"}`)
		c := &Client{BotToken: "t", BaseURL: srv.URL}
		_, err := c.Send(context.Background(), providers.ChatMessage{Address: "C1", Content: "x"})
		if err == nil {
			t.Fatalf("%s: expected error, got nil", code)
		}
		if providers.IsTransient(err) {
			t.Errorf("%s classified transient, want permanent: %v", code, err)
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("error %q missing slack error code %q", err, code)
		}
	}
}

// TestClient_Send_OKFalseTransient: HTTP 200 with ok=false and a known-transient
// error code is wrapped as TransientError.
func TestClient_Send_OKFalseTransient(t *testing.T) {
	for _, code := range []string{"ratelimited", "internal_error", "service_unavailable"} {
		srv, _ := stubServer(t, http.StatusOK, `{"ok":false,"error":"`+code+`"}`)
		c := &Client{BotToken: "t", BaseURL: srv.URL}
		_, err := c.Send(context.Background(), providers.ChatMessage{Address: "C1", Content: "x"})
		if err == nil {
			t.Fatalf("%s: expected error, got nil", code)
		}
		if !providers.IsTransient(err) {
			t.Errorf("%s classified permanent, want transient: %v", code, err)
		}
	}
}

// TestClient_Send_MissingBotTokenIsPermanent: no bot token fails closed.
func TestClient_Send_MissingBotTokenIsPermanent(t *testing.T) {
	c := &Client{BaseURL: "http://unused"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "C1", Content: "x"})
	if err == nil {
		t.Fatal("Send with no bot token: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing bot token classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingChannelIsPermanent: no Address fails closed.
func TestClient_Send_MissingChannelIsPermanent(t *testing.T) {
	c := &Client{BotToken: "t"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "x"})
	if err == nil {
		t.Fatal("Send with no channel: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing channel classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_5xxIsTransient: a 5xx HTTP status is wrapped as TransientError.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)

	c := &Client{BotToken: "t", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "C1", Content: "x"})
	if err == nil {
		t.Fatal("Send on 503: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("503 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx HTTP status is a permanent error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `bad`)

	c := &Client{BotToken: "t", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "C1", Content: "x"})
	if err == nil {
		t.Fatal("Send on 400: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("400 classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_TimeoutIsTransient: a client-side timeout is transient.
func TestClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &Client{BotToken: "t", BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "C1", Content: "x"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
