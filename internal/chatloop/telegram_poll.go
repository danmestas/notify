package chatloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// tgUpdate is one Telegram getUpdates result entry (only the fields the loop
// needs). update_id drives the offset; message.text carries the body.
type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
	} `json:"message"`
}

type tgUpdatesResponse struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

// telegramPollTimeout is the long-poll timeout (seconds) sent to getUpdates.
const telegramPollTimeout = 25

// runTelegramPoll long-polls getUpdates, tracking the offset (last update_id+1),
// and feeds each text message into handleInbound. Bot-authored messages are
// ignored. Telegram drops an update only once a later getUpdates call carries
// an offset past it, so the last batch stays pending until the next poll; the
// offset is therefore persisted (as a decimal string) after each dispatched
// batch and loaded at start, which closes the window where a restart re-reads
// that last batch. The offset only saves re-reading: each update_id is also
// claimed in the store before dispatch (claimInbound), so a message gets at
// most one agent turn even if the process dies between dispatch and the save. The loop runs until ctx is cancelled; a request error backs off
// briefly before retrying so a transient outage does not hot-spin.
func (s *Service) runTelegramPoll(ctx context.Context, scope domain.Scope) {
	base := s.telegramBaseURL
	if base == "" {
		base = telegramDefaultBaseURL
	}
	hc := s.httpClient
	if hc == nil {
		// Allow long-poll timeout + slack; per-request context still bounds it.
		hc = &http.Client{Timeout: (telegramPollTimeout + 10) * time.Second}
	}

	var offset int64
	if saved := s.loadPollCursor(ctx, scope, providerTelegram); saved != "" {
		if n, err := strconv.ParseInt(saved, 10, 64); err != nil {
			s.log.Warn("chatloop: bad telegram poll cursor; starting empty", "cursor", saved, "err", err)
		} else {
			offset = n
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := s.telegramGetUpdates(ctx, hc, base, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("chatloop: telegram getUpdates", "err", err)
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		batchStart := offset
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			if u.Message.From.IsBot {
				continue
			}
			chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
			subscriberID := "tg:" + strconv.FormatInt(u.Message.From.ID, 10)
			if !s.claimInbound(ctx, scope, providerTelegram, strconv.FormatInt(u.UpdateID, 10)) {
				continue
			}
			s.handleInbound(ctx, scope, providerTelegram, chatID, subscriberID, u.Message.Text)
		}
		if offset != batchStart {
			s.savePollCursor(ctx, scope, providerTelegram, strconv.FormatInt(offset, 10))
		}
	}
}

// telegramGetUpdates performs one long-poll. It bounds the request with a
// context whose deadline exceeds the server-side long-poll timeout.
func (s *Service) telegramGetUpdates(ctx context.Context, hc *http.Client, base string, offset int64) ([]tgUpdate, error) {
	reqCtx, cancel := context.WithTimeout(ctx, (telegramPollTimeout+10)*time.Second)
	defer cancel()

	q := url.Values{}
	q.Set("timeout", strconv.Itoa(telegramPollTimeout))
	if offset > 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	endpoint := strings.TrimRight(base, "/") + "/bot" + s.telegramToken + "/getUpdates?" + q.Encode()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: build getUpdates: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		// hc.Do returns a *url.Error embedding the request URL, which carries
		// the inline bot token (/bot<token>/getUpdates). Redact before wrapping
		// so neither the returned error nor anything logging it leaks the token.
		return nil, redactToken(fmt.Errorf("telegram: getUpdates: %w", err), s.telegramToken)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("telegram: getUpdates status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out tgUpdatesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("telegram: decode getUpdates: %w", err)
	}
	return out.Result, nil
}

// redactToken returns an error whose message has every occurrence of token
// replaced with "<redacted>", so a leaked URL segment "/bot<token>/..." becomes
// "/bot<redacted>/...". It is the single error boundary for the Telegram bot
// token: net/http embeds the request URL (with the inline token) in the
// *url.Error it returns on transport faults, and that error is both returned and
// logged verbatim upstream. Applying this once at each hc.Do boundary keeps the
// token out of every downstream error string and log line. An empty token (no
// credential configured) is a no-op so we never redact the empty string.
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

// sleepCtx sleeps for d or returns false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
