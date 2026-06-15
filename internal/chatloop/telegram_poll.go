package chatloop

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
// ignored. The loop runs until ctx is cancelled; a request error backs off
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
			s.handleInbound(ctx, scope, providerTelegram, chatID, subscriberID, u.Message.Text)
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
		return nil, fmt.Errorf("telegram: getUpdates: %w", err)
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
