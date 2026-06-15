package signalwire

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.SMSProvider = (*Client)(nil)

// captured is one request observed by the stub SignalWire server.
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

// TestClient_Send_Wire pins the SignalWire LaML wire (Twilio-compatible): POST
// {space}/api/laml/2010-04-01/Accounts/{ProjectID}/Messages.json with HTTP Basic
// auth (ProjectID:Token), Content-Type application/x-www-form-urlencoded, and a
// From/To/Body form body. The provider message id is the response JSON "sid".
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusCreated, `{"sid":"SW1234567890","status":"queued"}`)

	c := &Client{
		ProjectID: "proj_test",
		Token:     "tok_secret",
		SpaceURL:  srv.URL, // httptest host stands in for the Space
		From:      "+15550001111",
	}
	res, err := c.Send(context.Background(), providers.SMSMessage{To: "+15557654321", Body: "notify CI is red"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "SW1234567890" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "SW1234567890")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/api/laml/2010-04-01/Accounts/proj_test/Messages.json"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
	}
	user, pass, ok := parseBasicAuth(got.header.Get("Authorization"))
	if !ok {
		t.Fatalf("Authorization header not Basic: %q", got.header.Get("Authorization"))
	}
	if user != "proj_test" || pass != "tok_secret" {
		t.Errorf("basic auth = %q:%q, want proj_test:tok_secret", user, pass)
	}
	form, err := url.ParseQuery(got.body)
	if err != nil {
		t.Fatalf("parse body form: %v", err)
	}
	if form.Get("From") != "+15550001111" {
		t.Errorf("From = %q, want +15550001111", form.Get("From"))
	}
	if form.Get("To") != "+15557654321" {
		t.Errorf("To = %q, want +15557654321", form.Get("To"))
	}
	if form.Get("Body") != "notify CI is red" {
		t.Errorf("Body = %q, want %q", form.Get("Body"), "notify CI is red")
	}
}

// TestSpaceEndpoint pins the URL construction for a bare Space host (no scheme):
// it must resolve to https and the LaML Messages path.
func TestSpaceEndpoint_BareHostIsHTTPS(t *testing.T) {
	got := spaceEndpoint("example.signalwire.com", "PROJ")
	want := "https://example.signalwire.com/api/laml/2010-04-01/Accounts/PROJ/Messages.json"
	if got != want {
		t.Errorf("spaceEndpoint = %q, want %q", got, want)
	}
}

// TestClient_Send_MissingCredentialsIsPermanent: no project id / token fails
// closed with a permanent error.
func TestClient_Send_MissingCredentialsIsPermanent(t *testing.T) {
	c := &Client{SpaceURL: "x.signalwire.com", From: "+1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
	if err == nil {
		t.Fatal("Send with no credentials: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing credentials classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingSpaceIsPermanent: no space url fails closed.
func TestClient_Send_MissingSpaceIsPermanent(t *testing.T) {
	c := &Client{ProjectID: "p", Token: "t", From: "+1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
	if err == nil {
		t.Fatal("Send with no space url: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing space url classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "empty space url") {
		t.Errorf("error %q missing empty-space cause", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusUnauthorized, `{"message":"Authenticate"}`)

	c := &Client{ProjectID: "p", Token: "bad", SpaceURL: srv.URL, From: "+1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
	if err == nil {
		t.Fatal("Send on 401: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("401 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 401") {
		t.Errorf("error %q missing status code", err)
	}
}

// TestClient_Send_5xxIsTransient: a 5xx is wrapped as TransientError.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadGateway, ``)

	c := &Client{ProjectID: "p", Token: "t", SpaceURL: srv.URL, From: "+1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
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

	c := &Client{ProjectID: "p", Token: "t", SpaceURL: srv.URL, From: "+1", HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}

// parseBasicAuth decodes an "Authorization: Basic ..." header into user/pass.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	req := &http.Request{Header: http.Header{"Authorization": []string{h}}}
	return req.BasicAuth()
}
