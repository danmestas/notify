package chatloop

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
)

// mastodonPollInterval is the default gap between notifications polls.
const mastodonPollInterval = 20 * time.Second

// mastodonPendingReply is what ReplyHook needs to address and thread a reply
// that chat_threads itself has no column for. Keyed by author acct on
// Service.mastodonPending; see the field doc on Service.
type mastodonPendingReply struct {
	statusID   string
	visibility string
}

// mastodonNotification is one /api/v1/notifications entry (only the fields
// the loop needs). Account is the notifying account (the mention's author);
// Status is nil for notification types that carry no status (follow, etc.) --
// those are skipped.
type mastodonNotification struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Account struct {
		Acct string `json:"acct"`
		Bot  bool   `json:"bot"`
	} `json:"account"`
	Status *struct {
		ID         string `json:"id"`
		Content    string `json:"content"`
		Visibility string `json:"visibility"`
	} `json:"status"`
}

// runMastodonPoll polls GET {baseURL}/api/v1/notifications?types[]=mention on
// a fixed ticker and feeds each mention into handleInbound. since_id is kept
// in memory only (mirroring runTelegramPoll's offset): a restart resets it to
// empty and Mastodon will replay already-seen mentions. Threading itself is
// idempotent -- handleInbound's chat_thread lookup is keyed on (provider,
// chat_id), so a replayed mention continues the same conversation rather than
// forking a new one -- but the replayed message is still handed to the
// responder as a fresh turn, so a replay after restart can produce one
// duplicate agent reply. That is the same tradeoff every poller in this
// package already makes; it is not specific to Mastodon.
func (s *Service) runMastodonPoll(ctx context.Context, scope domain.Scope, baseURL, token string) {
	hc := s.httpClient
	if hc == nil {
		// Base URL is operator-supplied (integrations table), so the
		// fallback client must guard against SSRF the same way
		// connectivity.deliveryClient() does for the outbound path.
		hc = httpaction.NewSSRFSafeClientWithTimeout(15 * time.Second)
	}
	interval := s.mastodonPollInterval
	if interval <= 0 {
		interval = mastodonPollInterval
	}

	var sinceID string
	for {
		if ctx.Err() != nil {
			return
		}
		notes, err := s.mastodonFetchMentions(ctx, hc, baseURL, token, sinceID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("chatloop: mastodon notifications poll", "err", redactToken(err, token))
		} else {
			for _, n := range notes {
				if n.Type != "mention" || n.Status == nil {
					continue
				}
				if mastodonIDGreater(n.ID, sinceID) {
					sinceID = n.ID
				}
				if n.Account.Bot {
					// Skip bot-authored mentions to avoid a bot-to-bot
					// auto-reply loop (mirrors telegram's IsBot check).
					continue
				}
				acct := n.Account.Acct
				if acct == "" {
					continue
				}
				text := mastodonPlainText(n.Status.Content)
				if text == "" {
					continue
				}
				s.mastodonMu.Lock()
				if s.mastodonPending == nil {
					s.mastodonPending = make(map[string]mastodonPendingReply)
				}
				s.mastodonPending[acct] = mastodonPendingReply{
					statusID:   n.Status.ID,
					visibility: n.Status.Visibility,
				}
				s.mastodonMu.Unlock()
				s.handleInbound(ctx, scope, providerMastodon, acct, "mastodon:"+acct, text)
			}
		}
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

// mastodonFetchMentions performs one notifications poll, scoped to mentions
// and bounded by sinceID (empty on the first call).
func (s *Service) mastodonFetchMentions(ctx context.Context, hc *http.Client, baseURL, token, sinceID string) ([]mastodonNotification, error) {
	q := url.Values{}
	q["types[]"] = []string{"mention"}
	if sinceID != "" {
		q.Set("since_id", sinceID)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/notifications?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("mastodon: build notifications request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, redactToken(fmt.Errorf("mastodon: notifications: %w", err), token)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, redactToken(fmt.Errorf("mastodon: notifications status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), token)
	}
	var out []mastodonNotification
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("mastodon: decode notifications: %w", err)
	}
	return out, nil
}

// mastodonIDGreater reports whether a > b as Mastodon snowflake-style
// notification/status ids: both are non-negative decimal-digit strings with
// no leading zeros, so a longer string is always numerically greater, and
// equal-length strings compare lexicographically. An empty b (no cursor yet)
// treats any non-empty a as greater.
func mastodonIDGreater(a, b string) bool {
	if b == "" {
		return a != ""
	}
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// mastodonBlockBoundary matches the handful of tags Mastodon actually uses as
// line/paragraph separators. Only these are replaced with a space before tag
// stripping; every other tag (the <a>/<span> wrapping a mention's "@handle",
// in particular) is dropped with no replacement so it does not split a
// mention's "@" from its handle.
var mastodonBlockBoundary = regexp.MustCompile(`(?i)</p>|<br\s*/?>|</li>`)

// mastodonHTMLTag matches one HTML tag, used to strip Mastodon's status HTML
// down to plain text without pulling in an HTML parser.
var mastodonHTMLTag = regexp.MustCompile(`<[^>]*>`)

// mastodonPlainText converts a Mastodon status's HTML content to plain text
// (block boundaries become a space, every other tag is dropped, entities are
// unescaped, whitespace is collapsed) and strips every leading "@acct"
// mention token -- Mastodon prefixes a reply's content with an @-mention for
// every participant still in the thread, not only the bot, so all of them are
// dropped rather than just the first.
func mastodonPlainText(statusHTML string) string {
	text := mastodonBlockBoundary.ReplaceAllString(statusHTML, " ")
	text = mastodonHTMLTag.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = strings.Join(strings.Fields(text), " ")
	for strings.HasPrefix(text, "@") {
		rest := strings.SplitN(text, " ", 2)
		if len(rest) < 2 {
			text = ""
			break
		}
		text = rest[1]
	}
	return strings.TrimSpace(text)
}
