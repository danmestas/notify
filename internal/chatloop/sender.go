package chatloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/providers/bluesky"
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

// mastodonSender posts a reply via the Mastodon REST API
// (POST /api/v1/statuses, form-encoded, Authorization: Bearer <token>). It
// mirrors the outbound mastodon provider's wire (internal/providers/mastodon)
// but is owned by chatloop because the reply path threads onto the specific
// mention that triggered the turn (in_reply_to_id) and matches its
// visibility, rather than always posting a fresh public status. baseURL and
// token come from the integrations-table credential resolved at Start (see
// Service.resolveCredentialPoller), not an env var.
type mastodonSender struct {
	token   string
	baseURL string
	hc      *http.Client
}

func newMastodonSender(token, baseURL string, hc *http.Client) *mastodonSender {
	if hc == nil {
		// baseURL is operator-supplied (integrations table), so the
		// fallback client must be SSRF-guarded like the outbound path's
		// connectivity.deliveryClient().
		hc = httpaction.NewSSRFSafeClientWithTimeout(10 * time.Second)
	}
	return &mastodonSender{token: token, baseURL: baseURL, hc: hc}
}

// send posts the reply addressed to authorAcct ("@acct <text>"), threaded
// onto inReplyToID when non-empty, with visibility matching the mention's
// (an empty visibility lets Mastodon apply its own default).
func (s *mastodonSender) send(ctx context.Context, authorAcct, inReplyToID, visibility, text string) error {
	if s.token == "" {
		return fmt.Errorf("mastodon: empty access token")
	}
	endpoint := strings.TrimRight(s.baseURL, "/") + "/api/v1/statuses"
	form := url.Values{}
	form.Set("status", "@"+authorAcct+" "+text)
	if inReplyToID != "" {
		form.Set("in_reply_to_id", inReplyToID)
	}
	if visibility != "" {
		form.Set("visibility", visibility)
	}
	headers := map[string]string{
		"Authorization": "Bearer " + s.token,
		"Content-Type":  "application/x-www-form-urlencoded",
	}
	// The access token only ever travels in the Authorization header here (never
	// the URL), so redaction is defensive rather than closing a known leak --
	// mirrored anyway to keep every provider's token handling identical.
	return redactToken(doSend(ctx, s.hc, endpoint, headers, []byte(form.Encode()), "mastodon"), s.token)
}

// blueskySender posts a threaded reply via the AT Protocol
// (com.atproto.repo.createRecord, collection app.bsky.feed.post). It reuses
// the outbound bluesky provider's *bluesky.Client (internal/providers/bluesky)
// so the session cache (accessJwt/refreshJwt/did) and refresh-on-401 retry
// logic is shared with -- not duplicated from -- the poller that fetches
// mentions on the same client. The client's credentials come from the
// integrations-table credential resolved at Start (see
// Service.resolveCredentialPoller), not an env var.
type blueskySender struct {
	client *bluesky.Client
}

func newBlueskySender(client *bluesky.Client) *blueskySender {
	return &blueskySender{client: client}
}

// send posts text as an app.bsky.feed.post record, threaded onto pending
// when it has an addressable parent (an empty pending -- the reply's pending
// entry was lost to a restart -- falls back to an unthreaded post, the same
// tradeoff mastodonSender.send makes when its pending map misses).
func (s *blueskySender) send(ctx context.Context, text string, pending blueskyPendingReply) error {
	if err := s.client.EnsureSession(ctx); err != nil {
		return err
	}
	did := s.client.DID()

	record := map[string]any{
		"$type":     "app.bsky.feed.post",
		"text":      text,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	if pending.parentURI != "" {
		record["reply"] = map[string]any{
			"root":   map[string]string{"uri": pending.rootURI, "cid": pending.rootCID},
			"parent": map[string]string{"uri": pending.parentURI, "cid": pending.parentCID},
		}
	}
	body, err := json.Marshal(map[string]any{
		"repo":       did,
		"collection": "app.bsky.feed.post",
		"record":     record,
	})
	if err != nil {
		return fmt.Errorf("bluesky: marshal reply record: %w", err)
	}

	_, err = s.client.AuthedRequest(ctx, http.MethodPost, "com.atproto.repo.createRecord", nil, body)
	return err
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
