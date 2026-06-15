// Package telegram implements providers.ChatProvider against the Telegram Bot
// API (https://api.telegram.org/bot<token>/sendMessage). mirroring the reference
// telegram.provider, it POSTs a JSON body
// {chat_id, text, parse_mode:"HTML", disable_web_page_preview:true}.
// The bot token is the integration credential; the chat_id is the
// per-subscriber address (ChatMessage.Address). 2xx is success; 4xx is a
// permanent (plain) error; 5xx and transport faults are wrapped in
// providers.TransientError so the delivery handler can ask the engine to retry
// per 0001 §10.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.ChatProvider = (*Client)(nil)

// defaultBaseURL is the Telegram Bot API host. Overridable in tests via BaseURL.
const defaultBaseURL = "https://api.telegram.org"

// Client posts chat messages through one Telegram bot. BotToken is the
// integration credential; the per-recipient chat_id arrives in
// ChatMessage.Address. BaseURL defaults to the public Telegram host and is
// overridable for tests. A nil HTTPClient gets a 10s-timeout default per Send.
type Client struct {
	BotToken   string
	BaseURL    string // defaults to https://api.telegram.org
	HTTPClient *http.Client
}

// Send POSTs {chat_id, text, parse_mode:"HTML", disable_web_page_preview:true}
// to {BaseURL}/bot{BotToken}/sendMessage (mirrors the reference telegram.provider).
// chat_id comes from msg.Address; an empty address or bot token fails closed
// (permanent). The provider message id is the result.message_id of Telegram's
// JSON acknowledgement (best-effort).
func (c *Client) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	if c.BotToken == "" {
		return providers.Result{}, fmt.Errorf("telegram: empty bot token")
	}
	if msg.Address == "" {
		return providers.Result{}, fmt.Errorf("telegram: empty chat_id")
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + "/bot" + c.BotToken + "/sendMessage"

	body, err := json.Marshal(map[string]any{
		"chat_id":                  msg.Address,
		"text":                     msg.Content,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	if err != nil {
		return providers.Result{}, fmt.Errorf("telegram: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe.
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("telegram: post sendMessage: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			OK     bool `json:"ok"`
			Result struct {
				MessageID int64 `json:"message_id"`
			} `json:"result"`
		}
		_ = json.Unmarshal(respBody, &ack)
		return providers.Result{ProviderMessageID: strconv.FormatInt(ack.Result.MessageID, 10)}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad token, bad chat_id, payload): permanent.
		return providers.Result{}, fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
