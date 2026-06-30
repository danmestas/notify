// Package resend implements providers.EmailProvider against the Resend email
// API (POST https://api.resend.com/emails). mirroring the reference resend.provider,
// it POSTs a JSON body {from,to,subject,html,text} with a Bearer apiKey and
// reads the returned JSON "id" as the provider message id. 2xx is success;
// 4xx is a permanent (plain) error; 5xx and transport faults are wrapped in
// providers.TransientError so the delivery handler can ask the engine to retry
// per 0001 §10.
package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.EmailProvider = (*Client)(nil)

// defaultBaseURL is the Resend API host. Overridable in tests via BaseURL.
const defaultBaseURL = "https://api.resend.com"

// Client sends email through Resend. APIKey is the integration credential
// (Bearer auth); From is the sender used when EmailMessage.From is empty. A nil
// HTTPClient gets a 10s-timeout default per Send.
type Client struct {
	APIKey     string
	From       string
	BaseURL    string // defaults to https://api.resend.com
	HTTPClient *http.Client
}

// emailPayload is the wire JSON shape POSTed to /emails.
// reply_to is a []string per the Resend v2 schema (accepts string or []string;
// []string is forward-compatible with multi-address Reply-To).
type emailPayload struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
	Text    string   `json:"text"`
	ReplyTo []string `json:"reply_to,omitempty"`
}

// Send POSTs a JSON body to {BaseURL}/emails with Bearer auth (APIKey),
// mirroring the reference resend.provider. msg.From overrides the configured From when
// non-empty. An empty APIKey, recipient (msg.To), or resolved sender fails
// closed (permanent). The provider message id is the "id" of Resend's JSON
// acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.EmailMessage) (providers.Result, error) {
	if c.APIKey == "" {
		return providers.Result{}, fmt.Errorf("resend: missing api key")
	}
	if msg.To == "" {
		return providers.Result{}, fmt.Errorf("resend: empty recipient")
	}
	from := msg.From
	if from == "" {
		from = c.From
	}
	if from == "" {
		return providers.Result{}, fmt.Errorf("resend: empty from")
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + "/emails"

	p := emailPayload{
		From:    from,
		To:      msg.To,
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
	}
	if msg.ReplyTo != "" {
		p.ReplyTo = []string{msg.ReplyTo}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return providers.Result{}, fmt.Errorf("resend: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("resend: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer so
		// the endpoint does not leak into error text persisted/logged.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("resend: post emails: %w", err))
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
			fmt.Errorf("resend: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad api key, invalid payload): permanent.
		return providers.Result{}, fmt.Errorf("resend: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
