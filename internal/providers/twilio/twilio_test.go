package twilio

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

// captured is one request observed by the stub Twilio server.
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

// TestClient_Send_Wire pins the exact Twilio wire (mirrors the reference
// twilio.provider): POST {base}/2010-04-01/Accounts/{AccountSID}/Messages.json
// with HTTP Basic auth (AccountSID:AuthToken), Content-Type
// application/x-www-form-urlencoded, and a From/To/Body form body. The provider
// message id is the response JSON "sid".
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusCreated, `{"sid":"SM1234567890","status":"queued"}`)

	c := &Client{
		AccountSID: "AC_test_sid",
		AuthToken:  "tok_secret",
		From:       "+15550001111",
		BaseURL:    srv.URL,
	}
	res, err := c.Send(context.Background(), providers.SMSMessage{To: "+15557654321", Body: "notify CI is red"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "SM1234567890" {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, "SM1234567890")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/2010-04-01/Accounts/AC_test_sid/Messages.json"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
	}
	// Basic auth: AccountSID:AuthToken.
	user, pass, ok := parseBasicAuth(got.header.Get("Authorization"))
	if !ok {
		t.Fatalf("Authorization header not Basic: %q", got.header.Get("Authorization"))
	}
	if user != "AC_test_sid" || pass != "tok_secret" {
		t.Errorf("basic auth = %q:%q, want AC_test_sid:tok_secret", user, pass)
	}
	// Form body From/To/Body.
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

// TestClient_Send_MessageFromOverrides: a non-empty SMSMessage.From overrides
// the configured From.
func TestClient_Send_MessageFromOverrides(t *testing.T) {
	srv, got := stubServer(t, http.StatusCreated, `{"sid":"SM2"}`)

	c := &Client{AccountSID: "AC", AuthToken: "t", From: "+15550000000", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.SMSMessage{
		To:   "+15551112222",
		From: "+15559998888",
		Body: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	form, _ := url.ParseQuery(got.body)
	if form.Get("From") != "+15559998888" {
		t.Errorf("From = %q, want override +15559998888", form.Get("From"))
	}
}

// TestClient_Send_MissingCredentialsIsPermanent: no account sid / auth token
// fails closed with a permanent error rather than POSTing.
func TestClient_Send_MissingCredentialsIsPermanent(t *testing.T) {
	c := &Client{From: "+1", BaseURL: "http://unused"}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
	if err == nil {
		t.Fatal("Send with no credentials: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing credentials classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingRecipientIsPermanent: no To fails closed (permanent).
func TestClient_Send_MissingRecipientIsPermanent(t *testing.T) {
	c := &Client{AccountSID: "AC", AuthToken: "t", From: "+1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{Body: "b"})
	if err == nil {
		t.Fatal("Send with no recipient: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing recipient classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "empty recipient") {
		t.Errorf("error %q missing empty-recipient cause", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `{"code":21211,"message":"Invalid 'To'"}`)

	c := &Client{AccountSID: "AC", AuthToken: "t", From: "+1", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
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

	c := &Client{AccountSID: "AC", AuthToken: "t", From: "+1", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
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

	c := &Client{AccountSID: "AC", AuthToken: "t", From: "+1", BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+2", Body: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_TransportErrorHidesAccountSID: a transport fault returns a
// *url.Error embedding the request URL, which carries the AccountSID in the
// path (/Accounts/{SID}/Messages.json). The returned (transient) error must
// NOT leak the AccountSID. The *url.Error layer is unwrapped before wrapping.
func TestClient_Send_TransportErrorHidesAccountSID(t *testing.T) {
	const sid = "ACsecretaccountsid0123456789abcdef"
	c := &Client{
		AccountSID: sid,
		AuthToken:  "tok",
		From:       "+15550000000",
		BaseURL:    "http://127.0.0.1:0",
		HTTPClient: &http.Client{Timeout: 1 * time.Second},
	}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+15551112222", Body: "hi"})
	if err == nil {
		t.Fatal("Send on transport fault: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("transport fault classified permanent, want transient: %v", err)
	}
	if strings.Contains(err.Error(), sid) {
		t.Errorf("error leaks AccountSID: %q", err.Error())
	}
}

// parseBasicAuth decodes an "Authorization: Basic ..." header into user/pass.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	req := &http.Request{Header: http.Header{"Authorization": []string{h}}}
	return req.BasicAuth()
}
