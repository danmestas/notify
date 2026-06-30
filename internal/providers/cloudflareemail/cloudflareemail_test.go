package cloudflareemail

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

// compile-time interface check.
var _ providers.EmailProvider = (*Client)(nil)

// capturedReq holds one request observed by the stub server.
type capturedReq struct {
	method     string
	path       string
	header     http.Header
	body       string
	parsedBody cfSendRequest
}

// stubServer returns an httptest server answering with status/respBody and a
// pointer to the last captured request.
func stubServer(t *testing.T, status int, respBody string) (*httptest.Server, *capturedReq) {
	t.Helper()
	got := &capturedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.path = r.URL.Path
		got.header = r.Header.Clone()
		got.body = string(b)
		_ = json.Unmarshal(b, &got.parsedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestClient_Send_Wire pins the exact Cloudflare Email Sending API wire:
// POST .../accounts/{id}/email/sending/send, Authorization Bearer,
// Content-Type application/json, from as a PLAIN STRING address (NOT an
// {"address":...} object — that object-without-name form is what the live API
// rejected with HTTP 400 / code 10001 invalid_request_schema), both html+text,
// reply_to snake_case. It also pins that result.message_id is surfaced as the
// ProviderMessageID.
func TestClient_Send_Wire(t *testing.T) {
	const wantMsgID = "<abc123@craftdesign.group>"
	respBody := `{"success":true,"result":{"message_id":"` + wantMsgID +
		`","delivered":["alice@example.com"],"permanent_bounces":[],"queued":[]},"errors":[]}`
	srv, got := stubServer(t, http.StatusOK, respBody)

	c := &Client{
		AccountID: "acct-001",
		APIToken:  "tk_secret",
		From:      "notify@example.com",
		BaseURL:   srv.URL,
	}
	res, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "alice@example.com",
		Subject: "Hello Alice",
		Text:    "plain text body",
		HTML:    "<p>html body</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The real Message-ID from result.message_id is surfaced.
	if res.ProviderMessageID != wantMsgID {
		t.Errorf("ProviderMessageID = %q, want %q", res.ProviderMessageID, wantMsgID)
	}

	// Method and path.
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/accounts/acct-001/email/sending/send"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}

	// Authorization header.
	if h := got.header.Get("Authorization"); h != "Bearer tk_secret" {
		t.Errorf("Authorization = %q, want %q", h, "Bearer tk_secret")
	}
	if h := got.header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", h)
	}

	// from must be a plain string, parsed straight from the typed body.
	if got.parsedBody.From != "notify@example.com" {
		t.Errorf("from = %q, want notify@example.com", got.parsedBody.From)
	}

	// Wire-level guard against the 10001 regression: the raw JSON must carry
	// from as a string (`"from":"notify@example.com"`), NOT an object
	// (`"from":{`). The object-without-name shape is exactly what the live
	// endpoint rejected with invalid_request_schema.
	if !strings.Contains(got.body, `"from":"notify@example.com"`) {
		t.Errorf("body must send from as a plain string, got: %s", got.body)
	}
	if strings.Contains(got.body, `"from":{`) {
		t.Errorf("body must NOT send from as an object (10001 regression), got: %s", got.body)
	}

	// Both html and text sent.
	if got.parsedBody.HTML != "<p>html body</p>" {
		t.Errorf("html = %q, want <p>html body</p>", got.parsedBody.HTML)
	}
	if got.parsedBody.Text != "plain text body" {
		t.Errorf("text = %q, want plain text body", got.parsedBody.Text)
	}

	// Subject and To.
	if got.parsedBody.Subject != "Hello Alice" {
		t.Errorf("subject = %q, want Hello Alice", got.parsedBody.Subject)
	}
	if got.parsedBody.To != "alice@example.com" {
		t.Errorf("to = %q, want alice@example.com", got.parsedBody.To)
	}
}

// TestClient_Send_ReplyToEmpty_OmittedFromBody verifies that an empty ReplyTo
// produces no reply_to field in the Cloudflare wire body (omitempty behaviour).
func TestClient_Send_ReplyToEmpty_OmittedFromBody(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":["bob@example.com"],"permanent_bounces":[],"queued":[]},"errors":[]}`
	srv, got := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "bob@example.com",
		Subject: "Reply test",
		Text:    "body",
		// ReplyTo is "" (zero value) — must be absent from the wire body.
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(got.body, "reply_to") {
		t.Errorf("reply_to unexpectedly present in body when ReplyTo is empty: %s", got.body)
	}
}

// TestClient_Send_ReplyToPresent_InWireBody verifies that a non-empty ReplyTo
// appears in the Cloudflare wire body as the snake_case field "reply_to".
// Cloudflare uses snake_case for this field (unlike some providers that use
// camelCase). The resend and ses providers have an equivalent test; this pins
// the CF wire shape.
func TestClient_Send_ReplyToPresent_InWireBody(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":["charlie@example.com"],"permanent_bounces":[],"queued":[]},"errors":[]}`
	srv, got := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "charlie@example.com",
		Subject: "Reply test",
		Text:    "body",
		ReplyTo: "support@example.com",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// reply_to must appear in the wire body with the snake_case key.
	if !strings.Contains(got.body, `"reply_to"`) {
		t.Errorf("reply_to absent from wire body: %s", got.body)
	}
	if got.parsedBody.ReplyTo != "support@example.com" {
		t.Errorf("reply_to = %q, want support@example.com", got.parsedBody.ReplyTo)
	}
}

// TestClient_Send_QueuedIsSuccess verifies that a recipient in "queued" (not
// "delivered") is still considered a success.
func TestClient_Send_QueuedIsSuccess(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":[],"permanent_bounces":[],"queued":["carol@example.com"]},"errors":[]}`
	srv, _ := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	res, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "carol@example.com",
		Subject: "Queue test",
		Text:    "body",
	})
	if err != nil {
		t.Fatalf("Send on queued: expected nil error, got %v", err)
	}
	if res.ProviderMessageID == "" {
		t.Errorf("ProviderMessageID is empty, want non-empty")
	}
}

// TestClient_Send_PermanentBounceOn200IsPermanent verifies that a recipient in
// "permanent_bounces" on an HTTP 200 is a permanent (non-transient) error.
func TestClient_Send_PermanentBounceOn200IsPermanent(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":[],"permanent_bounces":["dead@example.com"],"queued":[]},"errors":[]}`
	srv, _ := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "dead@example.com",
		Subject: "Bounce test",
		Text:    "body",
	})
	if err == nil {
		t.Fatal("Send on permanent_bounce: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("permanent_bounce classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "permanent bounce") {
		t.Errorf("error %q missing 'permanent bounce'", err)
	}
}

// TestClient_Send_UnrelatedBounceDoesNotFail verifies the bounce check is a
// membership test on msg.To: an unrelated address in permanent_bounces must
// not fail this recipient's send when the recipient itself was delivered.
func TestClient_Send_UnrelatedBounceDoesNotFail(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":["alice@example.com"],"permanent_bounces":["other@example.com"],"queued":[]},"errors":[]}`
	srv, _ := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "alice@example.com",
		Subject: "Membership test",
		Text:    "body",
	})
	if err != nil {
		t.Fatalf("Send with unrelated bounce: expected nil error, got %v", err)
	}
}

// TestClient_Send_429IsTransient verifies that HTTP 429 is a transient error.
func TestClient_Send_429IsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusTooManyRequests, `{"success":false}`)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "e@e.com",
		Subject: "Rate",
		Text:    "body",
	})
	if err == nil {
		t.Fatal("Send on 429: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("429 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_5xxIsTransient verifies that HTTP 5xx is a transient error.
func TestClient_Send_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable, ``)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "e@e.com",
		Subject: "Server error",
		Text:    "body",
	})
	if err == nil {
		t.Fatal("Send on 503: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("503 classified permanent, want transient: %v", err)
	}
}

// TestClient_Send_400IsPermanent verifies that HTTP 400 is a permanent error.
func TestClient_Send_400IsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest, `{"success":false,"errors":[{"code":1000,"message":"bad request"}]}`)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "e@e.com",
		Subject: "Bad",
		Text:    "body",
	})
	if err == nil {
		t.Fatal("Send on 400: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("400 classified transient, want permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Errorf("error %q missing status code", err)
	}
	// The endpoint URL embeds the account ID and the error text persists into
	// Message.ErrorText — it must not appear in the error string.
	if strings.Contains(err.Error(), "/accounts/") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error %q leaks the endpoint URL / account ID", err)
	}
}

// TestClient_Send_401IsPermanent verifies that HTTP 401 is a permanent error.
func TestClient_Send_401IsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusUnauthorized, `{"success":false}`)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "f@e.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "e@e.com",
		Subject: "Auth",
		Text:    "body",
	})
	if err == nil {
		t.Fatal("Send on 401: expected error, got nil")
	}
	if providers.IsTransient(err) {
		t.Errorf("401 classified transient, want permanent: %v", err)
	}
}

// TestClient_Send_TimeoutIsTransient verifies that a client-side timeout is transient.
func TestClient_Send_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		AccountID:  "a1",
		APIToken:   "t1",
		From:       "f@e.com",
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
	}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "e@e.com",
		Subject: "Timeout",
		Text:    "body",
	})
	if err == nil {
		t.Fatal("Send on timeout: expected error, got nil")
	}
	if !providers.IsTransient(err) {
		t.Errorf("timeout classified permanent, want transient: %v", err)
	}
	// Transport errors are unwrapped from *url.Error so the endpoint URL
	// (carrying the account ID) does not leak into error text.
	if strings.Contains(err.Error(), "/accounts/") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error %q leaks the endpoint URL / account ID", err)
	}
}

// TestClient_Send_HonorsMsgFrom verifies that a non-empty msg.From overrides
// Client.From in the wire payload (bug fix: previously the code hardcoded c.From).
func TestClient_Send_HonorsMsgFrom(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":["x@e.com"],"permanent_bounces":[],"queued":[]},"errors":[]}`
	srv, got := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "default@x.io", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "x@e.com",
		Subject: "override test",
		Text:    "body",
		From:    "override@x.io",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.parsedBody.From != "override@x.io" {
		t.Errorf("from = %q, want override@x.io (msg.From must win)", got.parsedBody.From)
	}
}

// TestClient_Send_MsgFromEmpty_FallsBackToClientFrom verifies that an empty
// msg.From falls back to Client.From (integration-level default).
func TestClient_Send_MsgFromEmpty_FallsBackToClientFrom(t *testing.T) {
	const respBody = `{"success":true,"result":{"delivered":["x@e.com"],"permanent_bounces":[],"queued":[]},"errors":[]}`
	srv, got := stubServer(t, http.StatusOK, respBody)

	c := &Client{AccountID: "a1", APIToken: "t1", From: "default@x.io", BaseURL: srv.URL}
	if _, err := c.Send(context.Background(), providers.EmailMessage{
		To:      "x@e.com",
		Subject: "fallback test",
		Text:    "body",
		// From is empty → should fall back to c.From
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.parsedBody.From != "default@x.io" {
		t.Errorf("from = %q, want default@x.io (Client.From fallback)", got.parsedBody.From)
	}
}
