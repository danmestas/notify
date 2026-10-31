package chatloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/bluesky"
)

// blueskyPollInterval is the default gap between notifications polls.
const blueskyPollInterval = 20 * time.Second

// blueskyPendingReply is what ReplyHook needs to thread a reply as a proper
// app.bsky.feed.post reply object ({root, parent}) -- chat_threads has no
// column for it. Keyed by author DID on Service.blueskyPending; see the
// field doc on Service. parentURI/parentCID address the mention itself (the
// post our reply is a direct reply to); rootURI/rootCID address the top of
// the thread (the mention itself when it was not already part of a reply
// chain).
type blueskyPendingReply struct {
	parentURI string
	parentCID string
	rootURI   string
	rootCID   string
}

// blueskyNotification is one app.bsky.notification.listNotifications entry
// (only the fields the loop needs).
type blueskyNotification struct {
	URI    string `json:"uri"`
	CID    string `json:"cid"`
	Reason string `json:"reason"`
	Author struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	} `json:"author"`
	Record struct {
		Text  string `json:"text"`
		Reply *struct {
			Root struct {
				URI string `json:"uri"`
				CID string `json:"cid"`
			} `json:"root"`
			Parent struct {
				URI string `json:"uri"`
				CID string `json:"cid"`
			} `json:"parent"`
		} `json:"reply"`
	} `json:"record"`
	IndexedAt string `json:"indexedAt"`
}

type blueskyListNotificationsResp struct {
	Notifications []blueskyNotification `json:"notifications"`
}

// runBlueskyPoll polls app.bsky.notification.listNotifications on a fixed
// ticker and feeds each mention/reply into handleInbound. Reprocessing is
// prevented with a lastSeenAt cursor over the lexicographically (and
// chronologically, since indexedAt is RFC3339) comparable indexedAt
// timestamps -- chosen over calling com.atproto.notification.updateSeen
// because our own cursor is authoritative and needs no extra round trip per
// poll. The cursor is persisted in the store after each dispatched batch and
// loaded at start, so a restart does not replay already-handled
// notifications. The cursor only saves re-reading: correctness comes from
// claiming each notification uri in the store before dispatch (claimInbound),
// so a mention gets at most one agent turn even if the process dies between
// dispatch and the cursor save.
//
// The notifications feed is returned newest-first, so each batch is walked
// in reverse (oldest to newest) both to dispatch turns in chronological
// order and so the cursor advances to the batch's true newest entry.
func (s *Service) runBlueskyPoll(ctx context.Context, scope domain.Scope, client *bluesky.Client) {
	interval := s.blueskyPollInterval
	if interval <= 0 {
		interval = blueskyPollInterval
	}

	lastSeenAt := s.loadPollCursor(ctx, scope, providerBluesky)
	for {
		if ctx.Err() != nil {
			return
		}
		notes, err := blueskyFetchNotifications(ctx, client)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("chatloop: bluesky notifications poll", "err", err)
		} else {
			retry := false
			for i := len(notes) - 1; i >= 0; i-- {
				n := notes[i]
				if n.Reason != "mention" && n.Reason != "reply" {
					continue
				}
				if n.IndexedAt == "" {
					// No timestamp means the cursor can never mark it seen —
					// dispatching would re-fire it on every poll. Skip it.
					continue
				}
				if lastSeenAt != "" && n.IndexedAt <= lastSeenAt {
					continue
				}
				did := n.Author.DID
				if did == "" {
					continue
				}
				if did == client.DID() {
					// Skip self-authored notifications to avoid a
					// bot-to-bot auto-reply loop (mirrors telegram's
					// IsBot check).
					continue
				}
				text := strings.TrimSpace(n.Record.Text)
				if text == "" {
					continue
				}

				dispatch, again := s.claimInbound(ctx, scope, providerBluesky, n.URI)
				retry = retry || again
				if !dispatch {
					continue
				}

				pending := blueskyPendingReply{parentURI: n.URI, parentCID: n.CID}
				if n.Record.Reply != nil {
					pending.rootURI = n.Record.Reply.Root.URI
					pending.rootCID = n.Record.Reply.Root.CID
				} else {
					// A fresh mention (not itself a reply) is the root of the
					// new subthread our reply starts.
					pending.rootURI = n.URI
					pending.rootCID = n.CID
				}
				s.blueskyMu.Lock()
				if s.blueskyPending == nil {
					s.blueskyPending = make(map[string]blueskyPendingReply)
				}
				s.blueskyPending[did] = pending
				s.blueskyMu.Unlock()

				s.handleInbound(ctx, scope, providerBluesky, did, "bluesky:"+did, text)
			}
			if !retry && len(notes) > 0 && notes[0].IndexedAt != "" && notes[0].IndexedAt > lastSeenAt {
				lastSeenAt = notes[0].IndexedAt
				s.savePollCursor(ctx, scope, providerBluesky, lastSeenAt)
			}
		}
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

// blueskyFetchNotifications performs one app.bsky.notification.listNotifications
// call via the shared session client (which handles login/refresh) and
// decodes the notifications list.
func blueskyFetchNotifications(ctx context.Context, client *bluesky.Client) ([]blueskyNotification, error) {
	// limit=100 (the lexicon maximum) instead of the server-default 50: the
	// loop reads a single page per poll, so a bigger page halves the backlog
	// dropped when notifications outrun the poll interval. Full cursor
	// pagination is a known v1 tradeoff.
	body, err := client.AuthedRequest(ctx, http.MethodGet, "app.bsky.notification.listNotifications", url.Values{"limit": {"100"}}, nil)
	if err != nil {
		return nil, err
	}
	var out blueskyListNotificationsResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("bluesky: decode notifications: %w", err)
	}
	return out.Notifications, nil
}
