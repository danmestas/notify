package chatloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// telegramSender posts a reply via the Telegram Bot API sendMessage. It mirrors
// the outbound telegram provider's wire ({chat_id, text, parse_mode:"HTML"})
// but is owned by chatloop because the reply path dials out with the loop's bot
// token rather than a per-scope integration credential.
type telegramSender struct {
	token   string
	baseURL string // defaults to https://api.telegram.org; overridable in tests
	hc      *http.Client
}

func newTelegramSender(token, baseURL string, hc *http.Client) *telegramSender {
	if baseURL == "" {
		baseURL = telegramDefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &telegramSender{token: token, baseURL: baseURL, hc: hc}
}

func (s *telegramSender) send(ctx context.Context, chatID, text string) error {
	if s.token == "" {
		return fmt.Errorf("telegram: empty bot token")
	}
	endpoint := strings.TrimRight(s.baseURL, "/") + "/bot" + s.token + "/sendMessage"
	body, err := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	if err != nil {
		return fmt.Errorf("telegram: marshal body: %w", err)
	}
	// The sendMessage URL embeds the inline bot token (/bot<token>/sendMessage),
	// so a transport fault wraps a *url.Error carrying it. Redact at this
	// boundary so the reply path never returns or logs the token.
	return redactToken(doSend(ctx, s.hc, endpoint, nil, body, "telegram"), s.token)
}

// discordSender posts a reply via the Discord Bot REST API
// (POST /channels/{channel_id}/messages with Authorization: Bot <token>). The
// outbound discord provider is webhook-only and replies to a fixed channel, so
// the two-way reply path uses the bot REST API to answer in the channel the
// user messaged. channel_id arrives as chatID.
type discordSender struct {
	token   string
	baseURL string // defaults to https://discord.com/api/v10; overridable in tests
	hc      *http.Client
}

func newDiscordSender(token, baseURL string, hc *http.Client) *discordSender {
	if baseURL == "" {
		baseURL = discordAPIBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &discordSender{token: token, baseURL: baseURL, hc: hc}
}

func (s *discordSender) send(ctx context.Context, channelID, text string) error {
	if s.token == "" {
		return fmt.Errorf("discord: empty bot token")
	}
	endpoint := strings.TrimRight(s.baseURL, "/") + "/channels/" + channelID + "/messages"
	body, err := json.Marshal(map[string]any{"content": text})
	if err != nil {
		return fmt.Errorf("discord: marshal body: %w", err)
	}
	return doSend(ctx, s.hc, endpoint, map[string]string{"Authorization": "Bot " + s.token}, body, "discord")
}

// doSend POSTs body as JSON and treats 2xx as success; any other status (or
// transport fault) is an error so the ReplyHook can log it.
func doSend(ctx context.Context, hc *http.Client, endpoint string, headers map[string]string, body []byte, who string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", who, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: post: %w", who, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: status %d: %s", who, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
