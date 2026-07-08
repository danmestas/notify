package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

// Interface conformance: the three typed clients implement their channel
// provider interfaces.
var (
	_ providers.EmailProvider = (*EmailClient)(nil)
	_ providers.PushProvider  = (*PushClient)(nil)
	_ providers.ChatProvider  = (*ChatClient)(nil)
)

// Every client literal below sets HTTPClient: &http.Client{} explicitly. The
// zero-value fallback (post()'s nil-client branch) is now the SSRF-safe
// client, which deliberately blocks loopback — but stubServer binds to
// 127.0.0.1, so functional tests must opt out of the guard the same way
// httpaction_test.go's seed() does. TestSSRFGuard-style tests in this package
// (see webhook_ssrf_test.go) exercise the guarded default directly.

// captured is one request observed by the stub webhook server.
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

// wantSig computes the expected X-Notify-Signature value: sha256=<hex hmac of
// the exact body bytes>.
func wantSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ─── EmailClient ─────────────────────────────────────────────────────────────

func TestEmailClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"wh-email-1"}`)

	c := &EmailClient{URL: srv.URL + "/hook", HTTPClient: &http.Client{}}
	msg := providers.EmailMessage{
		From:    "from@example.com",
		To:      "to@example.com",
		Subject: "hi",
		Text:    "body text",
		HTML:    "<b>body</b>",
		ReplyTo: "reply@example.com",
	}
	res, err := c.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "wh-email-1" {
		t.Errorf("ProviderMessageID = %q, want wh-email-1", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/hook" {
		t.Errorf("path = %q, want /hook", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if sig := got.header.Get("X-Notify-Signature"); sig != "" {
		t.Errorf("X-Notify-Signature set without a secret: %q", sig)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, got.body)
	}
	for k, want := range map[string]string{
		"from":     "from@example.com",
		"to":       "to@example.com",
		"subject":  "hi",
		"text":     "body text",
		"html":     "<b>body</b>",
		"reply_to": "reply@example.com",
	} {
		if payload[k] != want {
			t.Errorf("body[%q] = %v, want %q", k, payload[k], want)
		}
	}
}

func TestEmailClient_Send_HMACSigned(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)

	const secret = "super-secret-key"
	c := &EmailClient{URL: srv.URL, HMACSecret: secret, HTTPClient: &http.Client{}}
	if _, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.z", Subject: "s"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := wantSig(secret, got.body)
	if sig := got.header.Get("X-Notify-Signature"); sig != want {
		t.Errorf("X-Notify-Signature = %q, want %q", sig, want)
	}
}

func TestEmailClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)
	c := &EmailClient{URL: srv.URL, HTTPClient: &http.Client{}}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "a@b.c"})
	if err == nil {
		t.Fatal("Send on 503: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("503 classified permanent, want transient: %v", err)
	}
}

func TestEmailClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `{"error":"bad"}`)
	c := &EmailClient{URL: srv.URL, HTTPClient: &http.Client{}}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "a@b.c"})
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

func TestEmailClient_Send_EmptyURLIsPermanent(t *testing.T) {
	c := &EmailClient{}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "a@b.c"})
	if err == nil {
		t.Fatal("Send with empty url: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty url classified transient, want permanent: %v", err)
	}
}

func TestEmailClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &EmailClient{URL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "a@b.c"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}

// ─── PushClient ──────────────────────────────────────────────────────────────

func TestPushClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"wh-push-1"}`)

	c := &PushClient{URL: srv.URL + "/p", HTTPClient: &http.Client{}}
	msg := providers.PushMessage{
		Topic:    "topic1",
		Title:    "Title",
		Body:     "the body",
		Priority: 4,
		Tags:     []string{"a", "b"},
		Markdown: true,
	}
	res, err := c.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "wh-push-1" {
		t.Errorf("ProviderMessageID = %q, want wh-push-1", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/p" {
		t.Errorf("path = %q, want /p", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, got.body)
	}
	if payload["topic"] != "topic1" {
		t.Errorf("body[topic] = %v, want topic1", payload["topic"])
	}
	if payload["title"] != "Title" {
		t.Errorf("body[title] = %v, want Title", payload["title"])
	}
	if payload["body"] != "the body" {
		t.Errorf("body[body] = %v, want 'the body'", payload["body"])
	}
	if payload["priority"] != float64(4) {
		t.Errorf("body[priority] = %v, want 4", payload["priority"])
	}
	if payload["markdown"] != true {
		t.Errorf("body[markdown] = %v, want true", payload["markdown"])
	}
	tags, ok := payload["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("body[tags] = %v, want [a b]", payload["tags"])
	}
}

func TestPushClient_Send_HMACSigned(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)

	const secret = "push-secret"
	c := &PushClient{URL: srv.URL, HMACSecret: secret, HTTPClient: &http.Client{}}
	if _, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := wantSig(secret, got.body)
	if sig := got.header.Get("X-Notify-Signature"); sig != want {
		t.Errorf("X-Notify-Signature = %q, want %q", sig, want)
	}
}

func TestPushClient_Send_NoSecretNoSignature(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)
	c := &PushClient{URL: srv.URL, HTTPClient: &http.Client{}}
	if _, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sig := got.header.Get("X-Notify-Signature"); sig != "" {
		t.Errorf("X-Notify-Signature set without a secret: %q", sig)
	}
}

func TestPushClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadGateway, ``)
	c := &PushClient{URL: srv.URL, HTTPClient: &http.Client{}}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send on 502: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("502 classified permanent, want transient: %v", err)
	}
}

func TestPushClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusUnauthorized, ``)
	c := &PushClient{URL: srv.URL, HTTPClient: &http.Client{}}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send on 401: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("401 classified transient, want permanent: %v", err)
	}
}

func TestPushClient_Send_EmptyURLIsPermanent(t *testing.T) {
	c := &PushClient{}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send with empty url: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty url classified transient, want permanent: %v", err)
	}
}

// ─── ChatClient ──────────────────────────────────────────────────────────────

func TestChatClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"wh-chat-1"}`)

	c := &ChatClient{URL: srv.URL + "/c", HTTPClient: &http.Client{}}
	res, err := c.Send(context.Background(), providers.ChatMessage{Content: "hello chat"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "wh-chat-1" {
		t.Errorf("ProviderMessageID = %q, want wh-chat-1", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/c" {
		t.Errorf("path = %q, want /c", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, got.body)
	}
	if payload["content"] != "hello chat" {
		t.Errorf("body[content] = %v, want 'hello chat'", payload["content"])
	}
}

func TestChatClient_Send_HMACSigned(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)

	const secret = "chat-secret"
	c := &ChatClient{URL: srv.URL, HMACSecret: secret, HTTPClient: &http.Client{}}
	if _, err := c.Send(context.Background(), providers.ChatMessage{Content: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := wantSig(secret, got.body)
	if sig := got.header.Get("X-Notify-Signature"); sig != want {
		t.Errorf("X-Notify-Signature = %q, want %q", sig, want)
	}
}

func TestChatClient_Send_AddressOverridesURL(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)
	c := &ChatClient{URL: "http://should.not.be.used.invalid/hook", HTTPClient: &http.Client{}}
	if _, err := c.Send(context.Background(), providers.ChatMessage{
		Address: srv.URL + "/override",
		Content: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.path != "/override" {
		t.Errorf("path = %q, want /override", got.path)
	}
}

func TestChatClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusInternalServerError, ``)
	c := &ChatClient{URL: srv.URL, HTTPClient: &http.Client{}}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on 500: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("500 classified permanent, want transient: %v", err)
	}
}

func TestChatClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusNotFound, ``)
	c := &ChatClient{URL: srv.URL, HTTPClient: &http.Client{}}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send on 404: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("404 classified transient, want permanent: %v", err)
	}
}

func TestChatClient_Send_EmptyURLIsPermanent(t *testing.T) {
	c := &ChatClient{}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "b"})
	if err == nil {
		t.Fatal("Send with empty url: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty url classified transient, want permanent: %v", err)
	}
}
