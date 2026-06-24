package ses

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

// captured is one request observed by the stub SES server.
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

// fixedClock returns a deterministic time so the SigV4 Authorization header is
// reproducible across runs.
func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }
}

// TestClient_Send_Wire pins the SESv2 wire: POST {base}/v2/email/outbound-emails
// with a SigV4 Authorization header and a
// {FromEmailAddress,Destination:{ToAddresses},Content:{Simple:{Subject,Body}}}
// body. The provider message id is the response JSON "MessageId".
func TestClient_Send_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"MessageId":"0100018abc-msgid"}`)

	c := &Client{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
		From:            "no-reply@example.com",
		BaseURL:         srv.URL,
		now:             fixedClock(),
	}
	res, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "user@example.com",
		Subject: "notify CI is red",
		HTML:    "<p>build failed</p>",
		Text:    "build failed",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.ProviderMessageID != "0100018abc-msgid" {
		t.Errorf("ProviderMessageID = %q, want 0100018abc-msgid", res.ProviderMessageID)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/v2/email/outbound-emails" {
		t.Errorf("path = %q, want /v2/email/outbound-emails", got.path)
	}
	if auth := got.header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/...", auth)
	}
	if got.header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header missing")
	}

	var payload struct {
		FromEmailAddress string `json:"FromEmailAddress"`
		Destination      struct {
			ToAddresses []string `json:"ToAddresses"`
		} `json:"Destination"`
		Content struct {
			Simple struct {
				Subject struct {
					Data string `json:"Data"`
				} `json:"Subject"`
				Body struct {
					Text struct {
						Data string `json:"Data"`
					} `json:"Text"`
					HTML struct {
						Data string `json:"Data"`
					} `json:"Html"`
				} `json:"Body"`
			} `json:"Simple"`
		} `json:"Content"`
	}
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
		t.Fatalf("parse body json: %v (body=%s)", err, got.body)
	}
	if payload.FromEmailAddress != "no-reply@example.com" {
		t.Errorf("FromEmailAddress = %q, want no-reply@example.com", payload.FromEmailAddress)
	}
	if len(payload.Destination.ToAddresses) != 1 || payload.Destination.ToAddresses[0] != "user@example.com" {
		t.Errorf("ToAddresses = %v, want [user@example.com]", payload.Destination.ToAddresses)
	}
	if payload.Content.Simple.Subject.Data != "notify CI is red" {
		t.Errorf("Subject.Data = %q, want notify CI is red", payload.Content.Simple.Subject.Data)
	}
	if payload.Content.Simple.Body.Text.Data != "build failed" {
		t.Errorf("Body.Text.Data = %q, want build failed", payload.Content.Simple.Body.Text.Data)
	}
	if payload.Content.Simple.Body.HTML.Data != "<p>build failed</p>" {
		t.Errorf("Body.Html.Data = %q, want <p>build failed</p>", payload.Content.Simple.Body.HTML.Data)
	}
}

// TestClient_Send_MessageFromOverrides: a non-empty EmailMessage.From overrides
// the configured From.
func TestClient_Send_MessageFromOverrides(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"MessageId":"id2"}`)

	c := &Client{
		AccessKeyID:     "k",
		SecretAccessKey: "s",
		Region:          "us-east-1",
		From:            "default@example.com",
		BaseURL:         srv.URL,
		now:             fixedClock(),
	}
	if _, err := c.Send(context.Background(), providers.EmailMessage{
		To:   "user@example.com",
		From: "override@example.com",
		Text: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(got.body), &payload)
	if payload["FromEmailAddress"] != "override@example.com" {
		t.Errorf("FromEmailAddress = %q, want override@example.com", payload["FromEmailAddress"])
	}
}

// TestClient_Send_MissingCredentialsIsPermanent: missing access key fails closed.
func TestClient_Send_MissingCredentialsIsPermanent(t *testing.T) {
	c := &Client{SecretAccessKey: "s", Region: "us-east-1", From: "a@b.com"}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
	if err == nil {
		t.Fatal("Send with no access key: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("missing access key classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_MissingRecipientIsPermanent: no To fails closed (permanent).
func TestClient_Send_MissingRecipientIsPermanent(t *testing.T) {
	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", From: "a@b.com"}
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
	srv, _ := stubServer(t, http.StatusBadRequest, `{"message":"invalid sender"}`)

	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", From: "a@b.com", BaseURL: srv.URL, now: fixedClock()}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
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

	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", From: "a@b.com", BaseURL: srv.URL, now: fixedClock()}
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

	c := &Client{
		AccessKeyID:     "k",
		SecretAccessKey: "s",
		Region:          "us-east-1",
		From:            "a@b.com",
		BaseURL:         srv.URL,
		HTTPClient:      &http.Client{Timeout: 50 * time.Millisecond},
		now:             fixedClock(),
	}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y.com", Text: "b"})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
}
