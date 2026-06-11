// Package ntfy implements providers.PushProvider against an ntfy server
// (https://ntfy.sh, verified against ntfy 2.x): POST {baseURL}/{topic} with the
// message text as the body and title/priority/tags/markdown as X-* headers.
// 2xx is success; 4xx is a permanent (plain) error; 5xx and transport faults
// are wrapped in providers.TransientError so the delivery handler can ask the
// engine to retry per 0001 §10.
package ntfy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

// Client posts push notifications to one ntfy server with one access token.
// Fields mirror connectivity.NtfyConfig. A nil HTTPClient gets a 10s-timeout
// default per Send.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// Send posts msg to {BaseURL}/{topic}. Headers: Authorization (Bearer, when a
// token is configured), X-Title, X-Priority (1-5), X-Tags (comma-joined),
// X-Markdown when set; the body is the message text. The provider message id
// is the "id" field of ntfy's JSON acknowledgement (best-effort). An empty
// topic fails closed (permanent) rather than POSTing to the base URL root.
func (c *Client) Send(ctx context.Context, msg providers.PushMessage) (providers.Result, error) {
	if msg.Topic == "" {
		return providers.Result{}, fmt.Errorf("ntfy: empty topic")
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/" + url.PathEscape(msg.Topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(msg.Body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("ntfy: build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if msg.Title != "" {
		req.Header.Set("X-Title", msg.Title)
	}
	if msg.Priority != 0 {
		req.Header.Set("X-Priority", strconv.Itoa(msg.Priority))
	}
	if len(msg.Tags) > 0 {
		req.Header.Set("X-Tags", strings.Join(msg.Tags, ","))
	}
	if msg.Markdown {
		req.Header.Set("X-Markdown", "yes")
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe.
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("ntfy: post %s: %w", endpoint, err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &ack)
		return providers.Result{ProviderMessageID: ack.ID}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("ntfy: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body))),
		)
	default:
		// 4xx (auth, bad topic, payload): permanent — a plain error.
		return providers.Result{}, fmt.Errorf("ntfy: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
