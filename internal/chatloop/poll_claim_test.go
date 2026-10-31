package chatloop

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/bluesky"
	corestore "github.com/danmestas/notify/internal/store"
)

// preClaim marks ids as already handled, standing in for a previous process
// that dispatched them and died before saving its cursor.
func preClaim(t *testing.T, svc *Service, scope domain.Scope, provider string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := svc.core.ChatInbound().MarkHandled(context.Background(), scope, provider, id); err != nil {
			t.Fatalf("preClaim: %v", err)
		}
	}
}

func assertOnlyDispatched(t *testing.T, resp *recordingResponder, want string) {
	t.Helper()
	waitCount(t, resp, 1)
	settle()
	if resp.count() != 1 {
		t.Fatalf("dispatched %d turns, want exactly 1", resp.count())
	}
	if turn, _ := resp.last(); turn.Text != want {
		t.Fatalf("dispatched %q, want %q", turn.Text, want)
	}
}

func TestBlueskyPoll_ClaimSkipsHandledAndDuplicateInBatch(t *testing.T) {
	x := newBlueskyXRPCServer(t)
	mention := func(id string) map[string]any {
		return map[string]any{
			"uri": "at://did:plc:friend/app.bsky.feed.post/" + id, "cid": "bafy" + id,
			"reason": "mention", "author": map[string]string{"did": "did:plc:friend"},
			"record": map[string]any{"text": "text " + id}, "indexedAt": "2024-01-01T00:00:00.000Z",
		}
	}
	// m1 was handled by a previous process; m2 appears twice in one batch.
	x.setNotifications([]map[string]any{mention("m2"), mention("m2"), mention("m1")})

	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &recordingResponder{}
	client := &bluesky.Client{PDSURL: x.srv.URL, Identifier: "bot.example.social", AppPassword: "app-pw"}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withBlueskyPollInterval(20 * time.Millisecond)
	svc.bluesky = newBlueskySender(client)
	preClaim(t, svc, scope, "bluesky", "at://did:plc:friend/app.bsky.feed.post/m1")

	stop := runUntilStopped(func(ctx context.Context) { svc.runBlueskyPoll(ctx, scope, client) })
	defer stop()
	assertOnlyDispatched(t, resp, "text m2")
}

func TestMastodonPoll_ClaimSkipsHandledAndDuplicateInBatch(t *testing.T) {
	note := func(id string) string {
		return `{"id":"` + id + `","type":"mention","account":{"acct":"friend@example.social"},` +
			`"status":{"id":"` + id + `","content":"<p>msg ` + id + `</p>","visibility":"public"}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + note("201") + "," + note("201") + "," + note("200") + "]"))
	}))
	defer srv.Close()

	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &recordingResponder{}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withMastodonPollInterval(20 * time.Millisecond)
	svc.mastodon = newMastodonSender("tok", srv.URL, svc.httpClient)
	preClaim(t, svc, scope, "mastodon", "200")

	stop := runUntilStopped(func(ctx context.Context) { svc.runMastodonPoll(ctx, scope, srv.URL, "tok") })
	defer stop()
	assertOnlyDispatched(t, resp, "msg 201")
}

func TestTelegramPoll_ClaimSkipsHandledAndDuplicateInBatch(t *testing.T) {
	upd := func(id string) string {
		return `{"update_id":` + id + `,"message":{"text":"text ` + id + `","chat":{"id":555},"from":{"id":7,"is_bot":false}}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[` + upd("10") + "," + upd("11") + "," + upd("11") + `]}`))
		time.Sleep(10 * time.Millisecond)
	}))
	defer srv.Close()

	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &recordingResponder{}
	svc := New(core, resp, "bot-tok", "", "support", nil).
		withTelegramBaseURL(srv.URL).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second})
	preClaim(t, svc, scope, "telegram", "10")

	stop := runUntilStopped(func(ctx context.Context) { svc.runTelegramPoll(ctx, scope) })
	defer stop()
	assertOnlyDispatched(t, resp, "text 11")
}

// TestMastodonPoll_FailedClaimIsRetriedNextPoll proves a mention whose claim
// cannot be recorded is not lost: the cursor stays at the batch start, so the
// next poll re-reads the mention and dispatches it once the store recovers.
// The fake server honours since_id, so an advanced cursor would hide it.
func TestMastodonPoll_FailedClaimIsRetriedNextPoll(t *testing.T) {
	note := `{"id":"201","type":"mention","account":{"acct":"friend@example.social"},` +
		`"status":{"id":"201","content":"<p>msg 201</p>","visibility":"public"}}`
	polled := make(chan struct{}, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("since_id") == "201" {
			_, _ = w.Write([]byte("[]"))
		} else {
			_, _ = w.Write([]byte("[" + note + "]"))
		}
		select {
		case polled <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	path := t.TempDir() + "/notify.db"
	core, err := corestore.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	// A second connection installs a trigger that makes every claim fail.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_claim BEFORE INSERT ON chat_inbound_handled
		BEGIN SELECT RAISE(ABORT, 'claim unavailable'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	resp := &recordingResponder{}
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withMastodonPollInterval(20 * time.Millisecond)
	svc.mastodon = newMastodonSender("tok", srv.URL, svc.httpClient)

	stop := runUntilStopped(func(ctx context.Context) { svc.runMastodonPoll(ctx, scope, srv.URL, "tok") })
	defer stop()
	// Let a couple of polls fail their claim.
	<-polled
	<-polled
	if n := resp.count(); n != 0 {
		t.Fatalf("dispatched %d turns while claims fail, want 0", n)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_claim`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	assertOnlyDispatched(t, resp, "msg 201")
}
