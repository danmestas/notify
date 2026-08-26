// Package mastodon implements providers.ChatProvider against the Mastodon
// REST API (https://docs.joinmastodon.org/methods/statuses/#create). It POSTs
// a form-encoded body {status, visibility} to {BaseURL}/api/v1/statuses with
// a Bearer access token. 2xx is success; 4xx is a permanent (plain) error;
// 5xx and transport faults are wrapped in providers.TransientError so the
// delivery handler can ask the engine to retry per 0001 §10.
package mastodon

import (
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

// defaultVisibility is used when Visibility is unset, matching Mastodon's own
// default for a new status.
const defaultVisibility = "public"

// Client posts chat messages through one Mastodon instance/application.
// AccessToken is the integration credential (Bearer auth); Visibility is the
// default status visibility ("public", "unlisted", "private", "direct") and
// defaults to "public" when empty. A nil HTTPClient gets a 10s-timeout
// default per Send.
type Client struct {
	BaseURL     string
	AccessToken string
	Visibility  string
	HTTPClient  *http.Client
}

// Send POSTs {status, visibility} form-encoded to {BaseURL}/api/v1/statuses
// with an Authorization: Bearer <token> header. msg.Address, when non-empty,
// is treated as a Mastodon @handle and prepended to the status text
// ("@handle status") — with Visibility "direct" this addresses a DM to that
// handle; an empty Address leaves the status a broadcast. No Idempotency-Key
// header is sent: ChatMessage carries no field identifying "this
// notification" independent of delivery bookkeeping, so a key derived from
// Address+Content alone would collide two distinct notifications that render
// identically for the same destination (a recurring alert firing twice) and
// Mastodon would silently drop the second post as a duplicate. The delivery
// engine's own retry bookkeeping already bounds duplicate sends. An empty
// base URL or access token fails closed (permanent). The provider message id
// is the created status's id (best-effort).
func (c *Client) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	if c.BaseURL == "" {
		return providers.Result{}, fmt.Errorf("mastodon: empty base url")
	}
	if c.AccessToken == "" {
		return providers.Result{}, fmt.Errorf("mastodon: empty access token")
	}

	status := msg.Content
	if msg.Address != "" {
		status = msg.Address + " " + msg.Content
	}
	visibility := c.Visibility
	if visibility == "" {
		visibility = defaultVisibility
	}

	form := url.Values{}
	form.Set("status", status)
	form.Set("visibility", visibility)

	endpoint := strings.TrimRight(c.BaseURL, "/") + "/api/v1/statuses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return providers.Result{}, fmt.Errorf("mastodon: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Redact the access token in
		// case it ever ends up embedded in the wrapped error (e.g. a custom
		// RoundTripper echoing headers), mirroring telegram.redactToken (#102).
		return providers.Result{}, providers.NewTransientError(
			redactToken(fmt.Errorf("mastodon: post statuses: %w", err), c.AccessToken),
		)
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
			redactToken(fmt.Errorf("mastodon: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))), c.AccessToken),
		)
	default:
		// 4xx (bad token, bad handle, payload): permanent.
		return providers.Result{}, redactToken(
			fmt.Errorf("mastodon: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
			c.AccessToken,
		)
	}
}

// redactToken returns an error whose message has every occurrence of token
// replaced with "<redacted>". Mirrors telegram.redactToken (#102): the access
// token must never appear in an error string, since Message.ErrorText
// persists it and logging echoes it verbatim. An empty token (no credential
// configured) is a no-op so we never redact the empty string.
func redactToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	clean := strings.ReplaceAll(msg, token, "<redacted>")
	if clean == msg {
		return err
	}
	return errors.New(clean)
}
