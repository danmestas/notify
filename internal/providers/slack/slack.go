// Package slack implements providers.ChatProvider against the Slack Web API
// (POST https://slack.com/api/chat.postMessage). mirroring the reference
// slack.provider, it POSTs a JSON body {channel,text} with a Bearer bot token.
//
// Slack returns HTTP 200 even on logical failure, with a body of the form
// {"ok":false,"error":"..."}. Send therefore parses the body and treats
// ok=false as an error: a small set of known-transient Slack error codes
// (ratelimited, internal_error, service_unavailable) is wrapped in
// providers.TransientError so the delivery handler can ask the engine to retry
// per 0001 §10; every other ok=false error (invalid_auth, channel_not_found,
// …) is permanent. On ok=true the provider message id is the "ts" field.
// 5xx and transport faults are transient; non-200/non-5xx HTTP is permanent.
package slack

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

var _ providers.ChatProvider = (*Client)(nil)

// defaultBaseURL is the Slack Web API host. Overridable in tests via BaseURL.
const defaultBaseURL = "https://slack.com/api"

// transientSlackErrors is the set of Slack ok=false error codes that are
// retry-safe; everything else is permanent.
var transientSlackErrors = map[string]bool{
	"ratelimited":         true,
	"internal_error":      true,
	"service_unavailable": true,
}

// Client posts chat messages through one Slack bot. BotToken is the integration
// credential (Bearer auth). A nil HTTPClient gets a 10s-timeout default per
// Send.
type Client struct {
	BotToken   string
	BaseURL    string // defaults to https://slack.com/api
	HTTPClient *http.Client
}

// chatPayload is the wire JSON shape POSTed to /chat.postMessage.
type chatPayload struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// Send POSTs {channel,text} to {BaseURL}/chat.postMessage with Bearer auth
// (BotToken). The channel is ChatMessage.Address and the text is
// ChatMessage.Content. An empty BotToken or Address fails closed (permanent).
// The provider message id is the "ts" of Slack's JSON acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	if c.BotToken == "" {
		return providers.Result{}, fmt.Errorf("slack: missing bot token")
	}
	if msg.Address == "" {
		return providers.Result{}, fmt.Errorf("slack: empty channel")
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + "/chat.postMessage"

	body, err := json.Marshal(chatPayload{Channel: msg.Address, Text: msg.Content})
	if err != nil {
		return providers.Result{}, fmt.Errorf("slack: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.BotToken)

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
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("slack: post chat.postMessage: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// Slack signals HTTP-level faults with 5xx; those are retry-safe.
	if resp.StatusCode >= 500 {
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("slack: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	}
	// Any non-2xx, non-5xx HTTP status is a permanent client error.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providers.Result{}, fmt.Errorf("slack: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// HTTP 200: Slack carries the real outcome in the JSON body's "ok" flag.
	var ack struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := json.Unmarshal(respBody, &ack); err != nil {
		return providers.Result{}, fmt.Errorf("slack: decode response: %w", err)
	}
	if !ack.OK {
		if transientSlackErrors[ack.Error] {
			return providers.Result{}, providers.NewTransientError(fmt.Errorf("slack: api error: %s", ack.Error))
		}
		return providers.Result{}, fmt.Errorf("slack: api error: %s", ack.Error)
	}
	return providers.Result{ProviderMessageID: ack.TS}, nil
}
