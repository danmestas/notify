package whatsapp

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

// captured is one request observed by the stub Meta Graph server.
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

// TestClient_Send_Wire pins the exact WhatsApp wire: POST
// {base}/{phone_number_id}/messages with Authorization: Bearer <accessToken>,
// Content-Type application/json, and a
// {messaging_product,recipient_type,to,type,text:{preview_url,body}} body. The
// provider message id is response messages[0].id.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"messages":[{"id":"wamid.ABC123"}]}`)

	c := &Client{AccessToken: "EAAG_secret", PhoneNumberID: "109999", BaseURL: srv.URL}
	res, err := c.Send(context.Background(), providers.ChatMessage{
		Address: "15557654321",
		Content: "notify CI is red",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "wamid.ABC123" {
		t.Errorf("ProviderMessageID = %q, want wamid.ABC123", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/109999/messages" {
		t.Errorf("path = %q, want /109999/messages", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer EAAG_secret" {
		t.Errorf("Authorization = %q, want Bearer EAAG_secret", auth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("parse body json: %v", err)
	}
	if payload["messaging_product"] != "whatsapp" {
		t.Errorf("messaging_product = %q, want whatsapp", payload["messaging_product"])
	}
	if payload["recipient_type"] != "individual" {
		t.Errorf("recipient_type = %q, want individual", payload["recipient_type"])
	}
	if payload["to"] != "15557654321" {
		t.Errorf("to = %q, want 15557654321", payload["to"])
	}
	if payload["type"] != "text" {
		t.Errorf("type = %q, want text", payload["type"])
	}
	text, ok := payload["text"].(map[string]any)
	if !ok {
		t.Fatalf("text is not an object: %v", payload["text"])
	}
	if text["preview_url"] != false {
		t.Errorf("text.preview_url = %v, want false", text["preview_url"])
	}
	if text["body"] != "notify CI is red" {
		t.Errorf("text.body = %q, want notify CI is red", text["body"])
	}
}

// TestClient_Send_MissingAccessTokenIsPermanent: no access token fails closed.
func TestClient_Send_MissingAccessTokenIsPermanent(t *testing.T) {
	c := &Client{PhoneNumberID: "1", BaseURL: "http://unused"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "x"})
	if err == nil {
		t.Fatal("Send with no access token: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing access token classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingPhoneNumberIDIsPermanent: no phone number id fails closed.
func TestClient_Send_MissingPhoneNumberIDIsPermanent(t *testing.T) {
	c := &Client{AccessToken: "t", BaseURL: "http://unused"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "x"})
	if err == nil {
		t.Fatal("Send with no phone number id: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing phone number id classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingRecipientIsPermanent: no Address fails closed.
func TestClient_Send_MissingRecipientIsPermanent(t *testing.T) {
	c := &Client{AccessToken: "t", PhoneNumberID: "1"}
	_, err := c.Send(context.Background(), providers.ChatMessage{Content: "x"})
	if err == nil {
		t.Fatal("Send with no recipient: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing recipient classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `{"error":{"message":"invalid recipient"}}`)

	c := &Client{AccessToken: "t", PhoneNumberID: "1", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "x"})
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
	srv, _ := stubServer(t, http.StatusBadGateway, ``)

	c := &Client{AccessToken: "t", PhoneNumberID: "1", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "x"})
	if err == nil {
		t.Fatal("Send on 502: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("502 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_TimeoutIsTransient: a client-side timeout is transient.
func TestClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &Client{AccessToken: "t", PhoneNumberID: "1", BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.ChatMessage{Address: "1", Content: "x"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
