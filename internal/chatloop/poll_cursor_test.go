package chatloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/bluesky"
	corestore "github.com/danmestas/notify/internal/store"
)

// waitCursor blocks until the persisted cursor for provider equals want.
func waitCursor(t *testing.T, core *corestore.Store, scope domain.Scope, provider, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		got, err := core.ChatPollCursors().Get(context.Background(), scope, provider)
		if err != nil {
			t.Fatalf("cursor get: %v", err)
		}
		if got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s cursor = %q, want %q", provider, got, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitCount(t *testing.T, resp *recordingResponder, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for resp.count() < n {
		select {
		case <-deadline:
			t.Fatalf("dispatches = %d, want %d", resp.count(), n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// settle gives a poller several intervals to (wrongly) dispatch a replay.
func settle() { time.Sleep(250 * time.Millisecond) }

// runUntilStopped starts run in a goroutine and returns a stop func that
// cancels it and waits for it to exit.
func runUntilStopped(run func(ctx context.Context)) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestBlueskyPoll_CursorSurvivesRestart(t *testing.T) {
	x := newBlueskyXRPCServer(t)
	mention := func(id, at string) map[string]any {
		return map[string]any{
			"uri":       "at://did:plc:friend/app.bsky.feed.post/" + id,
			"cid":       "bafy" + id,
			"reason":    "mention",
			"author":    map[string]string{"did": "did:plc:friend"},
			"record":    map[string]any{"text": "text " + id},
			"indexedAt": at,
		}
	}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	start := func(resp *recordingResponder) func() {
		client := &bluesky.Client{PDSURL: x.srv.URL, Identifier: "bot.example.social", AppPassword: "app-pw"}
		svc := New(core, resp, "", "", "support", nil).
			withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
			withBlueskyPollInterval(20 * time.Millisecond)
		svc.bluesky = newBlueskySender(client)
		return runUntilStopped(func(ctx context.Context) { svc.runBlueskyPoll(ctx, scope, client) })
	}

	x.setNotifications([]map[string]any{mention("m1", "2024-01-01T00:00:00.000Z")})
	first := &recordingResponder{}
	stop := start(first)
	waitCount(t, first, 1)
	waitCursor(t, core, scope, "bluesky", "2024-01-01T00:00:00.000Z")
	stop()

	// Newest first, as the real feed returns it: m1 was already handled.
	x.setNotifications([]map[string]any{
		mention("m2", "2024-01-02T00:00:00.000Z"),
		mention("m1", "2024-01-01T00:00:00.000Z"),
	})
	second := &recordingResponder{}
	stop = start(second)
	defer stop()
	waitCount(t, second, 1)
	settle()
	if second.count() != 1 {
		t.Fatalf("restart dispatched %d turns, want only the new mention", second.count())
	}
	if turn, _ := second.last(); turn.Text != "text m2" {
		t.Fatalf("restart dispatched %q, want %q", turn.Text, "text m2")
	}
}

func TestMastodonPoll_CursorSurvivesRestart(t *testing.T) {
	var mu sync.Mutex
	ids := []string{"200"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since_id")
		mu.Lock()
		var out []map[string]any
		for i := len(ids) - 1; i >= 0; i-- { // newest first
			if !mastodonIDGreater(ids[i], since) {
				continue
			}
			out = append(out, map[string]any{
				"id": ids[i], "type": "mention",
				"account": map[string]any{"acct": "friend@example.social"},
				"status":  map[string]any{"id": ids[i], "content": "<p>msg " + ids[i] + "</p>", "visibility": "public"},
			})
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	start := func(resp *recordingResponder) func() {
		svc := New(core, resp, "", "", "support", nil).
			withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
			withMastodonPollInterval(20 * time.Millisecond)
		svc.mastodon = newMastodonSender("tok", srv.URL, svc.httpClient)
		return runUntilStopped(func(ctx context.Context) { svc.runMastodonPoll(ctx, scope, srv.URL, "tok") })
	}

	first := &recordingResponder{}
	stop := start(first)
	waitCount(t, first, 1)
	waitCursor(t, core, scope, "mastodon", "200")
	stop()

	mu.Lock()
	ids = append(ids, "201")
	mu.Unlock()
	second := &recordingResponder{}
	stop = start(second)
	defer stop()
	waitCount(t, second, 1)
	settle()
	if second.count() != 1 {
		t.Fatalf("restart dispatched %d turns, want only the new mention", second.count())
	}
	if turn, _ := second.last(); turn.Text != "msg 201" {
		t.Fatalf("restart dispatched %q, want %q", turn.Text, "msg 201")
	}
}

func TestTelegramPoll_OffsetSurvivesRestart(t *testing.T) {
	var mu sync.Mutex
	maxID := 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var offset int
		_, _ = fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
		mu.Lock()
		var updates []string
		for id := 10; id <= maxID; id++ {
			if id >= offset {
				updates = append(updates, fmt.Sprintf(
					`{"update_id":%d,"message":{"text":"text %d","chat":{"id":555},"from":{"id":7,"is_bot":false}}}`, id, id,
				))
			}
		}
		mu.Unlock()
		if len(updates) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"ok":true,"result":[`
		for i, u := range updates {
			if i > 0 {
				body += ","
			}
			body += u
		}
		_, _ = w.Write([]byte(body + `]}`))
	}))
	defer srv.Close()

	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	start := func(resp *recordingResponder) func() {
		svc := New(core, resp, "bot-tok", "", "support", nil).
			withTelegramBaseURL(srv.URL).
			withHTTPClient(&http.Client{Timeout: 5 * time.Second})
		return runUntilStopped(func(ctx context.Context) { svc.runTelegramPoll(ctx, scope) })
	}

	first := &recordingResponder{}
	stop := start(first)
	waitCount(t, first, 1)
	waitCursor(t, core, scope, "telegram", "11")
	stop()

	// The server drops update 10 once offset 11 is confirmed to it, but a
	// restart forgot the offset; the persisted one must resume at 11.
	mu.Lock()
	maxID = 11
	mu.Unlock()
	second := &recordingResponder{}
	stop = start(second)
	defer stop()
	waitCount(t, second, 1)
	settle()
	if second.count() != 1 {
		t.Fatalf("restart dispatched %d turns, want only the new update", second.count())
	}
	if turn, _ := second.last(); turn.Text != "text 11" {
		t.Fatalf("restart dispatched %q, want %q", turn.Text, "text 11")
	}
}
