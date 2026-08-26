package chatloop

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/bluesky"
)

// blueskyXRPCServer is a fake AT Protocol PDS driving runBlueskyPoll /
// blueskySender end to end: createSession + refreshSession for the shared
// client's login/refresh cache, listNotifications for the poller, and
// createRecord for the reply sender.
type blueskyXRPCServer struct {
	t   *testing.T
	srv *httptest.Server

	createSessions  atomic.Int64
	refreshes       atomic.Int64
	listCalls       atomic.Int64
	createRecords   atomic.Int64
	unauthorizeNext atomic.Int64 // when >0, the next listNotifications/createRecord call 401s and decrements

	mu             sync.Mutex
	notifications  []map[string]any
	lastRecordBody map[string]any
}

func newBlueskyXRPCServer(t *testing.T) *blueskyXRPCServer {
	t.Helper()
	x := &blueskyXRPCServer{t: t}
	x.srv = httptest.NewServer(http.HandlerFunc(x.handle))
	t.Cleanup(x.srv.Close)
	return x
}

func (x *blueskyXRPCServer) setNotifications(notes []map[string]any) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.notifications = notes
}

func (x *blueskyXRPCServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "com.atproto.server.createSession"):
		x.createSessions.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessJwt":  "access-1",
			"refreshJwt": "refresh-1",
			"did":        "did:plc:bot",
		})
	case strings.HasSuffix(r.URL.Path, "com.atproto.server.refreshSession"):
		x.refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessJwt":  "access-2",
			"refreshJwt": "refresh-2",
			"did":        "did:plc:bot",
		})
	case strings.HasSuffix(r.URL.Path, "app.bsky.notification.listNotifications"):
		x.listCalls.Add(1)
		if n := x.unauthorizeNext.Load(); n > 0 {
			x.unauthorizeNext.Add(-1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"ExpiredToken"}`))
			return
		}
		x.mu.Lock()
		notes := x.notifications
		x.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"notifications": notes})
	case strings.HasSuffix(r.URL.Path, "com.atproto.repo.createRecord"):
		x.createRecords.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		x.mu.Lock()
		x.lastRecordBody = body
		x.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uri": "at://did:plc:bot/app.bsky.feed.post/reply1",
			"cid": "bafyreply1",
		})
	default:
		x.t.Fatalf("unexpected path: %s", r.URL.Path)
	}
}

func (x *blueskyXRPCServer) recordBody() map[string]any {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.lastRecordBody
}

// TestBlueskyPoll_MentionFeedsTextAndThreadsReply drives one fresh mention
// (not itself a reply) through runBlueskyPoll and then fires ReplyHook,
// proving the reply is posted as a createRecord with reply.parent AND
// reply.root both pointing at the mention (since it started the subthread).
func TestBlueskyPoll_MentionFeedsTextAndThreadsReply(t *testing.T) {
	x := newBlueskyXRPCServer(t)
	x.setNotifications([]map[string]any{
		{
			"uri":    "at://did:plc:friend/app.bsky.feed.post/m1",
			"cid":    "bafym1",
			"reason": "mention",
			"author": map[string]string{"did": "did:plc:friend", "handle": "friend.bsky.social"},
			"record": map[string]any{
				"text": "hey @bot what do you think?",
			},
			"indexedAt": "2024-01-01T00:00:00.000Z",
		},
	})

	resp := &recordingResponder{}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	client := &bluesky.Client{PDSURL: x.srv.URL, Identifier: "bot.example.social", AppPassword: "app-pw"}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withBlueskyPollInterval(20 * time.Millisecond)
	svc.bluesky = newBlueskySender(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runBlueskyPoll(ctx, scope, client)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for resp.count() < 1 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for mention dispatch")
		case <-time.After(10 * time.Millisecond):
		}
	}

	turn, _ := resp.last()
	if turn.Text != "hey @bot what do you think?" {
		t.Fatalf("text = %q, want unchanged mention text", turn.Text)
	}
	if turn.SubscriberID != "bluesky:did:plc:friend" {
		t.Fatalf("subscriber = %q, want %q", turn.SubscriberID, "bluesky:did:plc:friend")
	}

	// recordingResponder always mints "conv-x".
	svc.ReplyHook(ctx, scope, agentdomain.Conversation{PlatformThreadID: "conv-x"}, "here is my answer")

	deadline2 := time.After(2 * time.Second)
	for x.createRecords.Load() < 1 {
		select {
		case <-deadline2:
			t.Fatalf("reply createRecord never landed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	body := x.recordBody()
	if body["repo"] != "did:plc:bot" {
		t.Errorf("repo = %v, want did:plc:bot", body["repo"])
	}
	if body["collection"] != "app.bsky.feed.post" {
		t.Errorf("collection = %v, want app.bsky.feed.post", body["collection"])
	}
	record, _ := body["record"].(map[string]any)
	if record == nil || record["text"] != "here is my answer" {
		t.Fatalf("record = %+v, want text=here is my answer", body)
	}
	reply, _ := record["reply"].(map[string]any)
	if reply == nil {
		t.Fatalf("record missing reply object: %+v", record)
	}
	parent, _ := reply["parent"].(map[string]any)
	root, _ := reply["root"].(map[string]any)
	if parent["uri"] != "at://did:plc:friend/app.bsky.feed.post/m1" || parent["cid"] != "bafym1" {
		t.Errorf("reply.parent = %+v, want the mention's uri/cid", parent)
	}
	// A fresh mention (not itself a reply) is its own thread root.
	if root["uri"] != "at://did:plc:friend/app.bsky.feed.post/m1" || root["cid"] != "bafym1" {
		t.Errorf("reply.root = %+v, want the mention's uri/cid (fresh mention is its own root)", root)
	}

	cancel()
	<-done
}

// TestBlueskyPoll_ReplyNotificationPreservesThreadRoot proves a "reply"
// notification (the author replied within an existing thread) carries its
// own record.reply.root through to our reply's root, rather than treating
// the mention itself as the root.
func TestBlueskyPoll_ReplyNotificationPreservesThreadRoot(t *testing.T) {
	x := newBlueskyXRPCServer(t)
	x.setNotifications([]map[string]any{
		{
			"uri":    "at://did:plc:friend/app.bsky.feed.post/r1",
			"cid":    "bafyr1",
			"reason": "reply",
			"author": map[string]string{"did": "did:plc:friend", "handle": "friend.bsky.social"},
			"record": map[string]any{
				"text": "following up on that",
				"reply": map[string]any{
					"root":   map[string]string{"uri": "at://did:plc:bot/app.bsky.feed.post/root1", "cid": "bafyroot1"},
					"parent": map[string]string{"uri": "at://did:plc:bot/app.bsky.feed.post/p0", "cid": "bafyp0"},
				},
			},
			"indexedAt": "2024-01-01T00:00:00.000Z",
		},
	})

	resp := &recordingResponder{}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	client := &bluesky.Client{PDSURL: x.srv.URL, Identifier: "bot.example.social", AppPassword: "app-pw"}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withBlueskyPollInterval(20 * time.Millisecond)
	svc.bluesky = newBlueskySender(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runBlueskyPoll(ctx, scope, client)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for resp.count() < 1 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for mention dispatch")
		case <-time.After(10 * time.Millisecond):
		}
	}

	svc.ReplyHook(ctx, scope, agentdomain.Conversation{PlatformThreadID: "conv-x"}, "got it")

	deadline2 := time.After(2 * time.Second)
	for x.createRecords.Load() < 1 {
		select {
		case <-deadline2:
			t.Fatalf("reply createRecord never landed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	record, _ := x.recordBody()["record"].(map[string]any)
	reply, _ := record["reply"].(map[string]any)
	root, _ := reply["root"].(map[string]any)
	parent, _ := reply["parent"].(map[string]any)
	if root["uri"] != "at://did:plc:bot/app.bsky.feed.post/root1" || root["cid"] != "bafyroot1" {
		t.Errorf("reply.root = %+v, want the notification's own thread root", root)
	}
	// Our reply's parent is the notifying post itself, not its own parent.
	if parent["uri"] != "at://did:plc:friend/app.bsky.feed.post/r1" || parent["cid"] != "bafyr1" {
		t.Errorf("reply.parent = %+v, want the notifying post's uri/cid", parent)
	}

	cancel()
	<-done
}

// TestBlueskyPoll_SeenCursorPreventsReprocessing proves a notification whose
// indexedAt does not advance past the cursor is not dispatched twice, even
// though the fake server keeps returning it on every poll (mirroring a real
// PDS returning the same recent notifications window repeatedly).
func TestBlueskyPoll_SeenCursorPreventsReprocessing(t *testing.T) {
	x := newBlueskyXRPCServer(t)
	x.setNotifications([]map[string]any{
		{
			"uri":       "at://did:plc:friend/app.bsky.feed.post/m1",
			"cid":       "bafym1",
			"reason":    "mention",
			"author":    map[string]string{"did": "did:plc:friend", "handle": "friend.bsky.social"},
			"record":    map[string]any{"text": "hello"},
			"indexedAt": "2024-01-01T00:00:00.000Z",
		},
	})

	resp := &recordingResponder{}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	client := &bluesky.Client{PDSURL: x.srv.URL, Identifier: "bot.example.social", AppPassword: "app-pw"}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withBlueskyPollInterval(15 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runBlueskyPoll(ctx, scope, client)
		close(done)
	}()

	// Let several poll cycles pass so the same never-advancing notification
	// would be redelivered if the cursor were not working.
	deadline := time.After(2 * time.Second)
	for x.listCalls.Load() < 5 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for repeated polls (calls=%d)", x.listCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if resp.count() != 1 {
		t.Fatalf("dispatch count = %d, want exactly 1 (cursor should suppress replays)", resp.count())
	}
}

// TestBlueskyPoll_AuthFailureAtStartDisablesPollerWithoutCrashing proves that
// when the resolved bluesky credentials are incomplete, resolveCredentialPoller
// disables the poller (never launches the goroutine) instead of crashing
// Start or the surrounding loop -- mirroring the mastodon
// missing-base_url-or-token behaviour.
func TestBlueskyPoll_AuthFailureAtStartDisablesPollerWithoutCrashing(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	core := openTempStore(t)
	resp := &fakeResponder{}
	svc := New(core, resp, "", "", "support", logger).
		withCredentialSource(func(ctx context.Context, providerID string) (map[string]string, error) {
			if providerID == providerBluesky {
				// Missing app_password: incomplete credentials.
				return map[string]string{"pds_url": "https://example.social", "identifier": "bot.example.social"}, nil
			}
			return nil, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})

	out := buf.String()
	if !strings.Contains(out, "bluesky integration missing") {
		t.Errorf("log missing bluesky-missing-creds reason: %q", out)
	}
	if !strings.Contains(out, "poller disabled") {
		t.Errorf("log missing poller-disabled reason: %q", out)
	}
	if svc.bluesky != nil {
		t.Errorf("bluesky sender should not be wired when credentials are incomplete")
	}
}

// TestBlueskyPoll_RefreshOn401DuringPolling proves that a listNotifications
// call that 401s (an expired accessJwt) is transparently retried after a
// refresh by the shared client -- reusing bluesky.Client's own tested
// refresh-on-401 logic rather than re-implementing it in chatloop -- and the
// mention still reaches the responder.
func TestBlueskyPoll_RefreshOn401DuringPolling(t *testing.T) {
	x := newBlueskyXRPCServer(t)
	x.setNotifications([]map[string]any{
		{
			"uri":       "at://did:plc:friend/app.bsky.feed.post/m1",
			"cid":       "bafym1",
			"reason":    "mention",
			"author":    map[string]string{"did": "did:plc:friend", "handle": "friend.bsky.social"},
			"record":    map[string]any{"text": "hello again"},
			"indexedAt": "2024-01-01T00:00:00.000Z",
		},
	})
	// First mint a session, then force the poller's first listNotifications
	// call to 401 once it has one, exercising AuthedRequest's refresh retry.
	x.createSessions.Store(0)
	x.unauthorizeNext.Store(1)

	resp := &recordingResponder{}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	client := &bluesky.Client{PDSURL: x.srv.URL, Identifier: "bot.example.social", AppPassword: "app-pw"}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withBlueskyPollInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runBlueskyPoll(ctx, scope, client)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for resp.count() < 1 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for mention dispatch after refresh retry")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if x.refreshes.Load() < 1 {
		t.Errorf("refreshes = %d, want at least 1 (401 should have triggered a refresh)", x.refreshes.Load())
	}
}

// TestBlueskySender_RedactsAppPasswordAndJWTs proves neither the app password
// nor a minted JWT ever appears in an error returned by the reply sender,
// even when the PDS misbehaves in a way that would otherwise echo request
// internals back into the error (mirrors bluesky.Client's own redact test).
func TestBlueskySender_RedactsAppPasswordAndJWTs(t *testing.T) {
	const pw = "super-secret-reply-app-password"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(strings.Builder)
		_, _ = body.WriteString(`{"error":"BadRequest","message":"rejected: `)
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body.Write(buf[:n])
		body.WriteString(`"}`)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body.String()))
	}))
	defer srv.Close()

	client := &bluesky.Client{PDSURL: srv.URL, Identifier: "bot.example.social", AppPassword: pw}
	sender := newBlueskySender(client)

	err := sender.send(context.Background(), "hi", blueskyPendingReply{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("error leaks app password: %q", err.Error())
	}
}
