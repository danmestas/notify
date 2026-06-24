package sns

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

// captured is one request observed by the stub SNS server.
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

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }
}

const publishResponse = `<PublishResponse xmlns="http://sns.amazonaws.com/doc/2010-03-31/">
  <PublishResult>
    <MessageId>567910cd-659e-55d4-8ccb-5aaf14679dc0</MessageId>
  </PublishResult>
  <ResponseMetadata>
    <RequestId>d74b8436-ae13-5ab4-a9ff-ce54dfea72a0</RequestId>
  </ResponseMetadata>
</PublishResponse>`

// TestClient_Send_Wire pins the SNS query-API wire: POST {base}/ with
// content-type application/x-www-form-urlencoded and form
// Action=Publish&PhoneNumber=<E.164>&Message=<body>&Version=2010-03-31,
// SigV4-signed. The provider message id is parsed from the XML PublishResult.
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, publishResponse)

	c := &Client{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
		BaseURL:         srv.URL,
		now:             fixedClock(),
	}
	res, err := c.Send(context.Background(), providers.SMSMessage{
		To:   "+15557654321",
		Body: "notify CI is red",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "567910cd-659e-55d4-8ccb-5aaf14679dc0" {
		t.Errorf("ProviderMessageID = %q, want 567910cd-659e-55d4-8ccb-5aaf14679dc0", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/" {
		t.Errorf("path = %q, want /", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
	}
	if auth := got.header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/...", auth)
	}
	if got.header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header missing")
	}

	form, err := url.ParseQuery(got.body)
	if err != nil {
		t.Fatalf("parse form body: %v (body=%s)", err, got.body)
	}
	if form.Get("Action") != "Publish" {
		t.Errorf("Action = %q, want Publish", form.Get("Action"))
	}
	if form.Get("PhoneNumber") != "+15557654321" {
		t.Errorf("PhoneNumber = %q, want +15557654321", form.Get("PhoneNumber"))
	}
	if form.Get("Message") != "notify CI is red" {
		t.Errorf("Message = %q, want notify CI is red", form.Get("Message"))
	}
	if form.Get("Version") != "2010-03-31" {
		t.Errorf("Version = %q, want 2010-03-31", form.Get("Version"))
	}
}

// TestClient_Send_MissingCredentialsIsPermanent: missing access key fails closed.
func TestClient_Send_MissingCredentialsIsPermanent(t *testing.T) {
	c := &Client{SecretAccessKey: "s", Region: "us-east-1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+1", Body: "x"})
	if err == nil {
		t.Fatal("Send with no access key: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing access key classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingRecipientIsPermanent: no To fails closed (permanent).
func TestClient_Send_MissingRecipientIsPermanent(t *testing.T) {
	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1"}
	_, err := c.Send(context.Background(), providers.SMSMessage{Body: "x"})
	if err == nil {
		t.Fatal("Send with no recipient: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing recipient classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_4xxIsPermanent: a 4xx is a plain (non-transient) error.
func TestClient_Send_4xxIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `<ErrorResponse><Error><Message>invalid</Message></Error></ErrorResponse>`)

	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", BaseURL: srv.URL, now: fixedClock()}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+1", Body: "x"})
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

	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", BaseURL: srv.URL, now: fixedClock()}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+1", Body: "x"})
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

	c := &Client{
		AccessKeyID:     "k",
		SecretAccessKey: "s",
		Region:          "us-east-1",
		BaseURL:         srv.URL,
		HTTPClient:      &http.Client{Timeout: 50 * time.Millisecond},
		now:             fixedClock(),
	}
	_, err := c.Send(context.Background(), providers.SMSMessage{To: "+1", Body: "x"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
