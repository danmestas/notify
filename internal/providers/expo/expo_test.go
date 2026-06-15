package expo

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

// captured is one request observed by the stub Expo server.
type captured struct {
	method string
	path   string
	header http.Header
	body   string
}

// stubServer returns an httptest server answering status/respBody and a pointer
// to the last captured request.
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

// TestClient_Send_Wire pins the exact Expo wire (mirrors the reference expo.provider):
// POST {base} with a JSON array of one {to,title,body} message, Authorization
// Bearer when an access token is configured, and the provider message id taken
// from the status:"ok" ticket id.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK,
		`{"data":[{"status":"ok","id":"501b1c08-292a-41d7-a36e-461c223e4744"}]}`)

	c := &Client{BaseURL: srv.URL, AccessToken: "expo_secret"}
	res, err := c.Send(context.Background(), providers.PushMessage{
		Topic: "ExponentPushToken[xxxxxxxx]",
		Title: "Build failed",
		Body:  "notify CI is red",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "501b1c08-292a-41d7-a36e-461c223e4744" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "501b1c08-292a-41d7-a36e-461c223e4744")
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if h := got.header.Get("Authorization"); h != "Bearer expo_secret" {
		t.Errorf("Authorization = %q, want %q", h, "Bearer expo_secret")
	}
	if h := got.header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", h)
	}
	// Body must be a JSON array of one message with the device token in "to".
	wantSubstrs := []string{
		`"to":"ExponentPushToken[xxxxxxxx]"`,
		`"title":"Build failed"`,
		`"body":"notify CI is red"`,
	}
	if !strings.HasPrefix(strings.TrimSpace(got.body), "[") {
		t.Errorf("body is not a JSON array: %q", got.body)
	}
	for _, sub := range wantSubstrs {
		if !strings.Contains(got.body, sub) {
			t.Errorf("body %q missing %q", got.body, sub)
		}
	}
}

// TestClient_Send_OmitsAuthWhenNoToken: with no access token, no Authorization
// header is set (absent, not empty-valued).
func TestClient_Send_OmitsAuthWhenNoToken(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"data":[{"status":"ok","id":"x"}]}`)

	c := &Client{BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := got.header["Authorization"]; ok {
		t.Errorf("Authorization header present with no access token: %q", got.header.Get("Authorization"))
	}
}

// TestClient_Send_TicketErrorIsPermanent: a status:"error" ticket is a plain
// (non-transient) error carrying the Expo message.
func TestClient_Send_TicketErrorIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusOK,
		`{"data":[{"status":"error","message":"\"invalidDeviceToken\" is not a registered push notification recipient"}]}`)

	c := &Client{BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "invalidDeviceToken", Body: "b"})
	if err == nil {
		t.Fatal("Send on error ticket: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("ticket error classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "is not a registered push notification recipient") {
		t.Errorf("error %q missing Expo message", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `{"errors":[{"code":"PUSH_TOO_MANY_EXPERIENCE_IDS"}]}`)

	c := &Client{BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"})
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

	c := &Client{BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"})
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
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_EmptyTokenIsPermanent: an empty device token fails closed.
func TestClient_Send_EmptyTokenIsPermanent(t *testing.T) {
	c := &Client{BaseURL: "http://unused"}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "", Body: "b"})
	if err == nil {
		t.Fatal("Send with empty token: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty token classified transient, want permanent: %v", err)
	}
}
