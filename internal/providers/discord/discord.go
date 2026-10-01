// Package discord implements providers.ChatProvider against a Discord
// incoming webhook (https://discord.com/api/webhooks/{id}/{token}). Mirroring
// It POSTs a JSON body {"content": <text>} to the
// webhook URL with ?wait=true so Discord returns the created message object.
// 2xx is success; 4xx is a permanent (plain) error; 5xx and transport faults
// are wrapped in providers.TransientError so the delivery handler can ask the
// engine to retry per 0001 §10.
package discord

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

// Client posts chat messages to one Discord incoming webhook. WebhookURL is the
// integration credential and also the destination — Discord webhooks need no
// auth header and no per-recipient address. A nil HTTPClient gets a
// 10s-timeout default per Send.
type Client struct {
	WebhookURL string
	HTTPClient *http.Client
}

// Send POSTs {"content": msg.Content} to the webhook URL (msg.Address overrides
// the configured WebhookURL when non-empty, mirroring the subscriber-channel
// fallback in delivery/chat.go). The ?wait=true query asks Discord to return
// the created message so the provider message id can be captured (best-effort).
// An empty webhook URL fails closed (permanent).
func (c *Client) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	endpoint := c.WebhookURL
	if msg.Address != "" {
		endpoint = msg.Address
	}
	if endpoint == "" {
		return providers.Result{}, fmt.Errorf("discord: empty webhook url")
	}
	// Add ?wait=true so the webhook responds with the created message object,
	if u, err := url.Parse(endpoint); err == nil {
		q := u.Query()
		q.Set("wait", "true")
		u.RawQuery = q.Encode()
		endpoint = u.String()
	}

	body, err := json.Marshal(map[string]any{"content": msg.Content})
	if err != nil {
		return providers.Result{}, fmt.Errorf("discord: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer
		// so the webhook URL (which carries the secret token in its path,
		// /api/webhooks/{id}/{token}) does not leak into error text persisted
		// as Message.ErrorText or logged.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("discord: post webhook: %w", err))
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
			fmt.Errorf("discord: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad webhook, rate-limit body, payload): permanent.
		return providers.Result{}, fmt.Errorf("discord: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
