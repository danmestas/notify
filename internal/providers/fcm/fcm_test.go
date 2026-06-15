package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/providers"
	"golang.org/x/oauth2"
)

var _ providers.PushProvider = (*Client)(nil)

// stubTokenSource returns a fixed access token without reaching Google; tok
// is the token value and err (when set) simulates a mint failure.
type stubTokenSource struct {
	tok string
	err error
}

func (s stubTokenSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &oauth2.Token{AccessToken: s.tok}, nil
}

// captured is one request observed by the stub FCM server.
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

// TestClient_Send_Wire pins the FCM HTTP v1 wire (the end state the reference platform's
// firebase-admin-based provider produces): POST
// /v1/projects/{projectId}/messages:send, Authorization Bearer <oauth2 token>,
// body {"message":{token,notification:{title,body}}}, provider message id from
// the response "name".
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK,
		`{"name":"projects/test-proj/messages/0:1500415314455276"}`)

	c := &Client{
		ProjectID: "test-proj",
		Tokens:    stubTokenSource{tok: "ya29.fake_access_token"},
		BaseURL:   srv.URL,
	}
	res, err := c.Send(context.Background(), providers.PushMessage{
		Topic: "device-registration-token",
		Title: "Build failed",
		Body:  "notify CI is red",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "projects/test-proj/messages/0:1500415314455276" {
		t.Errorf("ProviderMessageID = %q, want the response name", res.ProviderMessageID)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/v1/projects/test-proj/messages:send" {
		t.Errorf("path = %q, want /v1/projects/test-proj/messages:send", got.path)
	}
	if h := got.header.Get("Authorization"); h != "Bearer ya29.fake_access_token" {
		t.Errorf("Authorization = %q, want Bearer ya29.fake_access_token", h)
	}
	if h := got.header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", h)
	}

	// Assert the exact v1 message structure.
	var sent fcmSendRequest
	if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v (body=%q)", err, got.body)
	}
	if sent.Message.Token != "device-registration-token" {
		t.Errorf("message.token = %q, want device-registration-token", sent.Message.Token)
	}
	if sent.Message.Notification.Title != "Build failed" {
		t.Errorf("message.notification.title = %q, want Build failed", sent.Message.Notification.Title)
	}
	if sent.Message.Notification.Body != "notify CI is red" {
		t.Errorf("message.notification.body = %q, want notify CI is red", sent.Message.Notification.Body)
	}
}

// TestClient_Send_TokenMintFailureIsTransient: a token-source failure is
// wrapped as TransientError and never reaches the HTTP endpoint.
func TestClient_Send_TokenMintFailureIsTransient(t *testing.T) {
	c := &Client{
		ProjectID: "test-proj",
		Tokens:    stubTokenSource{err: errors.New("oauth2: cannot fetch token")},
		BaseURL:   "http://unused",
	}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"})
	if err == nil {
		t.Fatal("Send on token-mint failure: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("token-mint failure classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx (e.g. UNREGISTERED) is a plain
// (non-transient) error carrying the response body.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest,
		`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid registration token"}}`)

	c := &Client{ProjectID: "p", Tokens: stubTokenSource{tok: "t"}, BaseURL: srv.URL}
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
	srv, _ := stubServer(t, http.StatusInternalServerError, ``)

	c := &Client{ProjectID: "p", Tokens: stubTokenSource{tok: "t"}, BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"})
	if err == nil {
		t.Fatal("Send on 500: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("500 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_TimeoutIsTransient: a client-side timeout is transient.
func TestClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		ProjectID:  "p",
		Tokens:     stubTokenSource{tok: "t"},
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
	}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "tok", Body: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_EmptyTokenIsPermanent: an empty device token fails closed
// without minting a token or hitting the network.
func TestClient_Send_EmptyTokenIsPermanent(t *testing.T) {
	c := &Client{ProjectID: "p", Tokens: stubTokenSource{tok: "t"}}
	_, err := c.Send(context.Background(), providers.PushMessage{Topic: "", Body: "b"})
	if err == nil {
		t.Fatal("Send with empty token: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("empty token classified transient, want permanent: %v", err)
	}
}

// TestNewClient_InvalidServiceAccountIsError: malformed SA JSON is a permanent
// configuration fault surfaced at construction.
func TestNewClient_InvalidServiceAccountIsError(t *testing.T) {
	_, err := NewClient(context.Background(), []byte(`{"not":"a service account"}`), "p")
	if err == nil {
		t.Fatal("NewClient with invalid SA JSON: expected error, got nil")
	}
}
