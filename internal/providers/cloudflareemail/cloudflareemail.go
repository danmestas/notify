// Package cloudflareemail implements providers.EmailProvider against the
// Cloudflare Email Sending API (v4), launched 2025:
//
//	POST /client/v4/accounts/{account_id}/email/sending/send
//
// Authorization: Bearer <token>. Both html and text bodies are sent when
// available. reply_to uses snake_case per the Cloudflare wire format.
//
// WIRE SHAPE: from/reply_to are sent as a PLAIN STRING (the bare address),
// not an object. The live endpoint rejects the object form when it carries
// only {"address": ...} with no "name": such a body returns HTTP 400
// {"code":10001,"message":"email.sending.error.invalid_request_schema"}.
// notify's sender is address-only (no display name), so the string form is
// both correct and the documented "simple send" shape. Confirmed against the
// live API: `{"from":"notify@craftdesign.group", ...}` → 200; the
// `{"from":{"address":"notify@craftdesign.group"}, ...}` object-without-name
// form → 400/10001. The object form is only accepted when a name is present.
//
// Retry classification per 0001 §10:
//
//	429 / 5xx → providers.TransientError (engine retries)
//	permanent_bounces in a 200 response → permanent error
//	400 / 401 / other 4xx → permanent error
package cloudflareemail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client sends email via the Cloudflare Email Sending API.
// Fields mirror connectivity.CloudflareEmailConfig.
// A nil HTTPClient gets a 10 s timeout default per Send.
// BaseURL may be overridden for tests; the zero value defaults to the live endpoint.
type Client struct {
	AccountID  string
	APIToken   string
	From       string // sender address, e.g. notify@example.com
	BaseURL    string // optional override; defaults to https://api.cloudflare.com/client/v4
	HTTPClient *http.Client
}

// cfSendRequest is the JSON body for POST …/email/sending/send.
//
// from and reply_to are plain-string addresses. The Cloudflare endpoint
// accepts either a bare string or an object {address, name}, but rejects an
// object that has address-only and no name (HTTP 400 / code 10001). Since
// notify sends an address with no display name, the string form is the
// schema-valid choice.
type cfSendRequest struct {
	To      string `json:"to"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	HTML    string `json:"html,omitempty"`
	Text    string `json:"text,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// cfSendResult is the result sub-object of the Cloudflare send response.
// MessageID is the RFC-5322 Message-ID the endpoint assigns to the send;
// the live API returns it at result.message_id on a 200.
type cfSendResult struct {
	MessageID        string   `json:"message_id"`
	Delivered        []string `json:"delivered"`
	PermanentBounces []string `json:"permanent_bounces"`
	Queued           []string `json:"queued"`
}

// cfSendResponse is the top-level response envelope.
type cfSendResponse struct {
	Success bool         `json:"success"`
	Result  cfSendResult `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Send dispatches msg via the Cloudflare Email Sending API.
//
// Success: the recipient appears in result.delivered or result.queued.
// Permanent bounce on HTTP 200: treat as a permanent (non-retryable) error.
// 429 and 5xx: wrap in providers.TransientError so the engine can retry.
// 400 / 401 and other 4xx: permanent error.
func (c *Client) Send(ctx context.Context, msg providers.EmailMessage) (providers.Result, error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	endpoint := strings.TrimRight(baseURL, "/") +
		"/accounts/" + url.PathEscape(c.AccountID) + "/email/sending/send"

	body := cfSendRequest{
		To:      msg.To,
		From:    c.From,
		Subject: msg.Subject,
	}
	if msg.HTML != "" {
		body.HTML = msg.HTML
	}
	if msg.Text != "" {
		body.Text = msg.Text
	}
	if msg.ReplyTo != "" {
		// reply_to is a bare address string (same wire shape as from). The
		// email two-way loop sets this to reply+<token>@<inbound-domain> so a
		// recipient reply routes back into the originating conversation.
		body.ReplyTo = msg.ReplyTo
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return providers.Result{}, fmt.Errorf("cloudflareemail: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return providers.Result{}, fmt.Errorf("cloudflareemail: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer
		// so the endpoint URL (which carries the account ID) does not leak
		// into error text persisted as Message.ErrorText.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("cloudflare email send: %w", err),
		)
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	// Transient: 429 rate-limit and 5xx server errors. Error text carries no
	// endpoint URL (it embeds the account ID and persists into Message.ErrorText).
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("cloudflare email send: status %d: %s",
				resp.StatusCode, strings.TrimSpace(string(rawBody))),
		)
	}

	// 4xx (other than 429): permanent.
	if resp.StatusCode >= 400 {
		return providers.Result{}, fmt.Errorf("cloudflare email send: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(rawBody)))
	}

	// 2xx: parse the structured response.
	var cfResp cfSendResponse
	if err := json.Unmarshal(rawBody, &cfResp); err != nil {
		return providers.Result{}, fmt.Errorf("cloudflareemail: unmarshal response: %w", err)
	}

	// The recipient appearing in permanent_bounces on HTTP 200 is a permanent
	// failure — do not retry. Membership check rather than len>0, so an
	// unrelated address in the list cannot fail this recipient's send.
	if slices.Contains(cfResp.Result.PermanentBounces, msg.To) {
		return providers.Result{}, fmt.Errorf(
			"cloudflareemail: permanent bounce for %s", msg.To,
		)
	}

	if !cfResp.Success {
		// success=false with 2xx and no permanent bounce: surface error codes.
		var codes []string
		for _, e := range cfResp.Errors {
			codes = append(codes, fmt.Sprintf("%d:%s", e.Code, e.Message))
		}
		return providers.Result{}, fmt.Errorf("cloudflareemail: api error: %s",
			strings.Join(codes, "; "))
	}

	// Success: prefer the real RFC-5322 Message-ID the API returns at
	// result.message_id. Older/edge responses may omit it (e.g. delivered/
	// queued only), so fall back to a synthetic id built from the recipient.
	if cfResp.Result.MessageID != "" {
		return providers.Result{ProviderMessageID: cfResp.Result.MessageID}, nil
	}
	return providers.Result{ProviderMessageID: "cf-email:" + msg.To}, nil
}
