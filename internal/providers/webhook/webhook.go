// Package webhook implements the generic-webhook delivery providers
// (email-webhook, push-webhook, chat-webhook). Each typed client POSTs the
// rendered message as a JSON body to a configured URL, optionally signing the
// body with HMAC-SHA256. mirroring the reference email/push/chat webhook providers,
// the signature is the HMAC-SHA256 of the exact JSON body bytes; it is sent as
// the X-Notify-Signature header in the form "sha256=<hex>" only when an HMAC
// secret is configured.
//
// 2xx is success; 4xx is a permanent (plain) error; 5xx and transport faults
// are wrapped in providers.TransientError so the delivery handler can ask the
// engine to retry per 0001 §10. All three clients share one deep post helper so
// the HTTP, signing, and status-mapping logic lives in exactly one place.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/providers"
)

// defaultHTTPClient is the SSRF-safe client used when a caller does not supply
// its own HTTPClient. URL/Address are tenant-supplied (the webhook endpoint
// configured for an integration, or ChatMessage.Address), so the fallback must
// not be a plain client: an attacker-controlled webhook URL pointing at an
// internal host must be rejected at dial time, not merely at request build.
var defaultHTTPClient = httpaction.NewSSRFSafeClientWithTimeout(10 * time.Second)

var (
	_ providers.EmailProvider = (*EmailClient)(nil)
	_ providers.PushProvider  = (*PushClient)(nil)
	_ providers.ChatProvider  = (*ChatClient)(nil)
)

// signatureHeader is the header carrying the HMAC-SHA256 of the request body.
const signatureHeader = "X-Notify-Signature"

// EmailClient posts a rendered email as JSON to a webhook URL. URL is the
// destination; HMACSecret, when set, signs the body. A nil HTTPClient gets a
// 10s-timeout default per Send.
type EmailClient struct {
	URL        string
	HMACSecret string
	HTTPClient *http.Client
}

// emailPayload is the wire JSON shape posted by EmailClient.
type emailPayload struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// Send POSTs msg as JSON to the configured URL.
func (c *EmailClient) Send(ctx context.Context, msg providers.EmailMessage) (providers.Result, error) {
	return post(ctx, c.HTTPClient, c.URL, c.HMACSecret, emailPayload{
		From:    msg.From,
		To:      msg.To,
		Subject: msg.Subject,
		Text:    msg.Text,
		HTML:    msg.HTML,
		ReplyTo: msg.ReplyTo,
	}, "email-webhook")
}

// PushClient posts a rendered push notification as JSON to a webhook URL.
type PushClient struct {
	URL        string
	HMACSecret string
	HTTPClient *http.Client
}

// pushPayload is the wire JSON shape posted by PushClient.
type pushPayload struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags,omitempty"`
	Markdown bool     `json:"markdown"`
}

// Send POSTs msg as JSON to the configured URL.
func (c *PushClient) Send(ctx context.Context, msg providers.PushMessage) (providers.Result, error) {
	return post(ctx, c.HTTPClient, c.URL, c.HMACSecret, pushPayload{
		Topic:    msg.Topic,
		Title:    msg.Title,
		Body:     msg.Body,
		Priority: msg.Priority,
		Tags:     msg.Tags,
		Markdown: msg.Markdown,
	}, "push-webhook")
}

// ChatClient posts a rendered chat message as JSON to a webhook URL. A non-empty
// ChatMessage.Address overrides the configured URL, mirroring the
// subscriber-channel fallback used by the other chat providers.
type ChatClient struct {
	URL        string
	HMACSecret string
	HTTPClient *http.Client
}

// chatPayload is the wire JSON shape posted by ChatClient.
type chatPayload struct {
	Content string `json:"content"`
}

// Send POSTs msg as JSON to msg.Address (when set) or the configured URL.
func (c *ChatClient) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	endpoint := c.URL
	if msg.Address != "" {
		endpoint = msg.Address
	}
	return post(ctx, c.HTTPClient, endpoint, c.HMACSecret, chatPayload{
		Content: msg.Content,
	}, "chat-webhook")
}

// post is the single deep helper shared by all three webhook clients: it
// JSON-marshals payload, optionally adds the HMAC-SHA256 signature header, POSTs
// to endpoint, and maps the response status to a transient/permanent outcome.
// 2xx → success (provider message id from a best-effort {"id":...} body); 5xx
// and transport faults → providers.TransientError; 4xx → a plain (permanent)
// error. An empty endpoint fails closed (permanent) rather than POSTing.
func post(ctx context.Context, hc *http.Client, endpoint, hmacSecret string, payload any, name string) (providers.Result, error) {
	if endpoint == "" {
		return providers.Result{}, fmt.Errorf("%s: empty webhook url", name)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return providers.Result{}, fmt.Errorf("%s: marshal body: %w", name, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("%s: build request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if hmacSecret != "" {
		req.Header.Set(signatureHeader, sign(hmacSecret, body))
	}

	if hc == nil {
		hc = defaultHTTPClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer so
		// the endpoint (which may carry a secret token in its path) does not leak
		// into error text persisted as Message.ErrorText or logged.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("%s: post webhook: %w", name, err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(respBody, &ack)
		return providers.Result{ProviderMessageID: ack.ID}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("%s: status %d: %s", name, resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad request, auth, payload): permanent.
		return providers.Result{}, fmt.Errorf("%s: status %d: %s", name, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// sign returns the X-Notify-Signature value for body: "sha256=<hex>" where hex
// is the HMAC-SHA256 of the exact body bytes keyed by secret. mirrors the reference
// computeHmac (createHmac('sha256', secret).update(body).digest('hex')).
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
