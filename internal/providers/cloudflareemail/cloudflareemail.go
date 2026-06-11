// Package cloudflareemail implements providers.EmailProvider against the
// Cloudflare Email Sending API (v4), launched 2025:
//
//	POST /client/v4/accounts/{account_id}/email/sending/send
//
// Authorization: Bearer <token>. Both html and text bodies are sent when
// available. reply_to uses snake_case per the Cloudflare wire format.
// from is sent as an object with address/name, not a flat string.
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

// cfFromAddress is the from/reply_to object shape the Cloudflare API requires.
// Cloudflare uses "address" NOT "email" for the sender address field.
type cfFromAddress struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

// cfSendRequest is the JSON body for POST …/email/sending/send.
type cfSendRequest struct {
	To      string         `json:"to"`
	From    cfFromAddress  `json:"from"`
	Subject string         `json:"subject"`
	HTML    string         `json:"html,omitempty"`
	Text    string         `json:"text,omitempty"`
	ReplyTo *cfFromAddress `json:"reply_to,omitempty"`
}

// cfSendResult is the result sub-object of the Cloudflare send response.
type cfSendResult struct {
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
		From:    cfFromAddress{Address: c.From},
		Subject: msg.Subject,
	}
	if msg.HTML != "" {
		body.HTML = msg.HTML
	}
	if msg.Text != "" {
		body.Text = msg.Text
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

	// Treat "delivered" and "queued" as success; return a synthetic message id
	// built from the recipient address since the API does not return a message id.
	return providers.Result{ProviderMessageID: "cf-email:" + msg.To}, nil
}
