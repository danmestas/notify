package resend

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

var _ providers.EmailProvider = (*Client)(nil)

// captured is one request observed by the stub Resend server.
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

// TestClient_Send_Wire pins the exact Resend wire (mirrors the reference
// resend.provider): POST {base}/emails with Authorization: Bearer <apiKey>,
// Content-Type application/json, and a {from,to,subject,html,text} body. The
// provider message id is the response JSON "id".
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"re_12345"}`)

	c := &Client{APIKey: "re_secret_key", From: "no-reply@example.com", BaseURL: srv.URL}
	res, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "user@example.com",
		Subject: "notify CI is red",
		HTML:    "<p>build failed</p>",
		Text:    "build failed",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "re_12345" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "re_12345")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/emails" {
		t.Errorf("path = %q, want /emails", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer re_secret_key" {
		t.Errorf("Authorization = %q, want Bearer re_secret_key", auth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("parse body json: %v", err)
	}
	if payload["from"] != "no-reply@example.com" {
		t.Errorf("from = %q, want no-reply@example.com", payload["from"])
	}
	if payload["to"] != "user@example.com" {
		t.Errorf("to = %q, want user@example.com", payload["to"])
	}
	if payload["subject"] != "notify CI is red" {
		t.Errorf("subject = %q, want notify CI is red", payload["subject"])
	}
	if payload["html"] != "<p>build failed</p>" {
		t.Errorf("html = %q, want <p>build failed</p>", payload["html"])
	}
	if payload["text"] != "build failed" {
		t.Errorf("text = %q, want build failed", payload["text"])
	}
}

// TestClient_Send_MessageFromOverrides: a non-empty EmailMessage.From overrides
// the configured From.
func TestClient_Send_MessageFromOverrides(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"re_2"}`)

	c := &Client{APIKey: "k", From: "default@example.com", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.EmailMessage{
		To:   "user@example.com",
		From: "override@example.com",
		Text: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(got.body), &payload)
	if payload["from"] != "override@example.com" {
		t.Errorf("from = %q, want override override@example.com", payload["from"])
	}
}

// TestClient_Send_MissingAPIKeyIsPermanent: no api key fails closed (permanent).
func TestClient_Send_MissingAPIKeyIsPermanent(t *testing.T) {
	c := &Client{From: "a@b.com", BaseURL: "http://unused"}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
	if err == nil {
		t.Fatal("Send with no api key: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing api key classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingRecipientIsPermanent: no To fails closed (permanent).
func TestClient_Send_MissingRecipientIsPermanent(t *testing.T) {
	c := &Client{APIKey: "k", From: "a@b.com"}
	_, err := c.Send(context.Background(), providers.EmailMessage{Text: "b"})
	if err == nil {
		t.Fatal("Send with no recipient: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing recipient classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusUnprocessableEntity, `{"message":"invalid from"}`)

	c := &Client{APIKey: "k", From: "a@b.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
	if err == nil {
		t.Fatal("Send on 422: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("422 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 422") {
		t.Errorf("error %q missing status code", err)
	}
}

// TestClient_Send_5xxIsTransient: a 5xx is wrapped as TransientError.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)

	c := &Client{APIKey: "k", From: "a@b.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
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

	c := &Client{APIKey: "k", From: "a@b.com", BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_ReplyTo verifies that msg.ReplyTo is sent as reply_to in the
// Resend wire payload (as a JSON array per Resend v2 schema).
func TestClient_Send_ReplyTo(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"re_reply"}`)

	c := &Client{APIKey: "k", From: "a@b.com", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "x@y.com",
		Text:    "body",
		ReplyTo: "support@x.io",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(got.body), &payload)
	raw, ok := payload["reply_to"]
	if !ok {
		t.Fatalf("reply_to field missing from wire body: %s", got.body)
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != 1 || arr[0] != "support@x.io" {
		t.Errorf("reply_to = %v, want [\"support@x.io\"]", raw)
	}
}

// TestClient_Send_ReplyToEmpty_OmittedFromWire verifies that an empty
// msg.ReplyTo does not emit a reply_to key in the wire body.
func TestClient_Send_ReplyToEmpty_OmittedFromWire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"re_no_reply"}`)

	c := &Client{APIKey: "k", From: "a@b.com", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.EmailMessage{
		To:   "x@y.com",
		Text: "body",
		// ReplyTo is empty — must not appear in wire body.
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(got.body, "reply_to") {
		t.Errorf("reply_to unexpectedly present in body: %s", got.body)
	}
}
