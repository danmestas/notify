package telegram

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

// TestClient_Send_Wire pins the exact Telegram Bot API wire: POST
// {base}/bot{token}/sendMessage with JSON {chat_id, text, parse_mode:"HTML",
// disable_web_page_preview:true}.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"ok":true,"result":{"message_id":4242,"date":1700000000}}`)

	c := &Client{BotToken: "test-bot-token-123", BaseURL: srv.URL}
	res, err := c.Send(context.Background(), providers.ChatMessage{
		Address: "987654321",
		Content: "Hello from notify",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "4242" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "4242")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/bottest-bot-token-123/sendMessage" {
		t.Errorf("path = %q, want /bottest-bot-token-123/sendMessage", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, got.body)
	}
	if payload["chat_id"] != "987654321" {
		t.Errorf("chat_id = %v, want %q", payload["chat_id"], "987654321")
	}
	if payload["text"] != "Hello from notify" {
		t.Errorf("text = %v, want %q", payload["text"], "Hello from notify")
	}
	if payload["parse_mode"] != "HTML" {
		t.Errorf("parse_mode = %v, want HTML", payload["parse_mode"])
	}
	if payload["disable_web_page_preview"] != true {
		t.Errorf("disable_web_page_preview = %v, want true", payload["disable_web_page_preview"])
	}
}

// TestClient_Send_TokenInURL: the bot token appears in the request path.
func TestClient_Send_TokenInURL(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"ok":true,"result":{"message_id":1}}`)

	c := &Client{BotToken: "abc:XYZ", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(got.path, "/botabc:XYZ/") {
		t.Errorf("path = %q, want bot token in path", got.path)
	}
}

// TestClient_Send_TransportErrorRedactsToken: a transport fault returns a
// *url.Error embedding the request URL, which carries the inline bot token
// (/bot<token>/sendMessage). The returned (transient) error must NOT leak the
// token and must carry the redaction marker. Twin of the chatloop fix (#102).
func TestClient_Send_TransportErrorRedactsToken(t *testing.T) {
	const token = "secret-bot-token-987:ABCdef"
	// Point at an unroutable address so hc.Do fails at the transport layer
	// before any server is contacted; the *url.Error then embeds the full URL.
	c := &Client{
		BotToken:   token,
		BaseURL:    "http://127.0.0.1:0",
		HTTPClient: &http.Client{Timeout: 1 * time.Second},
	}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "b"})
	if err == nil {
		t.Fatal("Send on transport fault: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("transport fault classified permanent, want transient: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks bot token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("error missing redaction marker: %q", err.Error())
	}
}

// TestClient_Send_EmptyChatIDIsPermanent: no address fails closed (permanent).
func TestClient_Send_EmptyChatIDIsPermanent(t *testing.T) {
	c := &Client{BotToken: "t"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send with empty chat_id: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty chat_id classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_EmptyTokenIsPermanent: no bot token fails closed (permanent).
func TestClient_Send_EmptyTokenIsPermanent(t *testing.T) {
	c := &Client{}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "b"})
	if err == nil {
		t.Fatal("Send with empty token: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty token classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `{"ok":false,"description":"chat not found"}`)

	c := &Client{BotToken: "t", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "b"})
	if err == nil {
		t.Fatal("Send on 400: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("400 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Errorf("error %q missing status code", err)
	}
}

// TestClient_Send_5xxIsTransient: a 5xx is wrapped as TransientError.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)

	c := &Client{BotToken: "t", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "b"})
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

	c := &Client{BotToken: "t", BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
