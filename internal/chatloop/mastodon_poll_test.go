package chatloop

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

// TestMastodonPoll_MentionFeedsStrippedTextAndThreadsReply drives one mention
// through runMastodonPoll (stripped text lands at the responder, HTML and the
// leading @bot mention removed) and then fires ReplyHook to prove the reply
// posts back with the mention's status id as in_reply_to_id, the author
// prefixed onto the status, and the mention's own visibility carried through.
func TestMastodonPoll_MentionFeedsStrippedTextAndThreadsReply(t *testing.T) {
	var mu sync.Mutex
	var sinceIDs []string
	var served int

	var replyMu sync.Mutex
	var replyStatus, replyInReplyTo, replyVisibility, replyAuth string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sinceIDs = append(sinceIDs, r.URL.Query().Get("since_id"))
		served++
		n := served
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`[{"id":"200","type":"mention","account":{"acct":"friend@example.social"},
				"status":{"id":"200","content":"<p><span class=\"h-card\"><a href=\"x\">@<span>bot</span></a></span> hello there</p>","visibility":"public"}}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		replyMu.Lock()
		replyStatus = vals.Get("status")
		replyInReplyTo = vals.Get("in_reply_to_id")
		replyVisibility = vals.Get("visibility")
		replyAuth = r.Header.Get("Authorization")
		replyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"999"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := &recordingResponder{}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	const token = "test-mastodon-token"
	svc := New(core, resp, "", "", "support", nil).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withMastodonPollInterval(20 * time.Millisecond)
	// Wire the reply sender the way resolveCredentialPoller does, without
	// going through Start/CredentialSource (out of scope for this test).
	svc.mastodon = newMastodonSender(token, srv.URL, svc.httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runMastodonPoll(ctx, scope, srv.URL, token)
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
	if turn.Text != "hello there" {
		t.Fatalf("stripped text = %q, want %q", turn.Text, "hello there")
	}
	if turn.SubscriberID != "mastodon:friend@example.social" {
		t.Fatalf("subscriber = %q, want %q", turn.SubscriberID, "mastodon:friend@example.social")
	}

	// Fire the reply hook the way ingress would after the agent replies.
	// recordingResponder always mints "conv-x", so that is the thread id
	// handleInbound bound the chat to.
	svc.ReplyHook(ctx, scope, agentdomain.Conversation{PlatformThreadID: "conv-x"}, "here is the answer")

	deadline2 := time.After(2 * time.Second)
	for {
		replyMu.Lock()
		got := replyStatus
		replyMu.Unlock()
		if got != "" {
			break
		}
		select {
		case <-deadline2:
			t.Fatalf("reply POST never landed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	replyMu.Lock()
	gotStatus, gotReplyTo, gotVis, gotAuth := replyStatus, replyInReplyTo, replyVisibility, replyAuth
	replyMu.Unlock()

	if want := "@friend@example.social here is the answer"; gotStatus != want {
		t.Errorf("reply status = %q, want %q", gotStatus, want)
	}
	if gotReplyTo != "200" {
		t.Errorf("in_reply_to_id = %q, want 200", gotReplyTo)
	}
	if gotVis != "public" {
		t.Errorf("visibility = %q, want public", gotVis)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+token)
	}

	// since_id must advance to 200 on a later poll.
	deadline3 := time.After(2 * time.Second)
	for {
		mu.Lock()
		var sawAdvance bool
		snapshot := append([]string(nil), sinceIDs...)
		for _, s := range snapshot {
			if s == "200" {
				sawAdvance = true
			}
		}
		mu.Unlock()
		if sawAdvance {
			break
		}
		select {
		case <-deadline3:
			t.Fatalf("since_id never advanced to 200; saw %v", snapshot)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// TestMastodonPoll_401KeepsLoopAliveAndRedactsToken proves a 401 from the
// notifications endpoint is logged (not fatal), the loop keeps retrying
// instead of crashing, no turn is dispatched, and the access token never
// appears in a log line.
func TestMastodonPoll_401KeepsLoopAliveAndRedactsToken(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"The access token is invalid"}`))
	}))
	defer srv.Close()

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resp := &recordingResponder{}
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	const token = "super-secret-mastodon-token"
	svc := New(core, resp, "", "", "support", logger).
		withHTTPClient(&http.Client{Timeout: 5 * time.Second}).
		withMastodonPollInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.runMastodonPoll(ctx, scope, srv.URL, token)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("loop did not retry after 401 (calls=%d)", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if resp.count() != 0 {
		t.Fatalf("expected no dispatch on 401, got %d", resp.count())
	}
	out := buf.String()
	if strings.Contains(out, token) {
		t.Fatalf("log leaked mastodon access token: %q", out)
	}
	if !strings.Contains(out, "status 401") {
		t.Errorf("log missing 401 status: %q", out)
	}
}

// syncBuffer used above is defined once in telegram_redact_test.go and shared
// across the package's poller tests.

// TestMastodonPlainText strips HTML, entities, and every leading @mention
// (Mastodon prefixes a reply with one @mention per thread participant).
func TestMastodonPlainText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single leading mention",
			in:   `<p><span class="h-card"><a href="x">@<span>bot</span></a></span> hello there</p>`,
			want: "hello there",
		},
		{
			name: "multiple leading mentions in a thread reply",
			in:   `<p><a href="a">@bot</a> <a href="b">@alice</a> thanks for the help!</p>`,
			want: "thanks for the help!",
		},
		{
			name: "entities unescaped",
			in:   `<p>@bot 5 &gt; 3 &amp; true</p>`,
			want: "5 > 3 & true",
		},
		{
			name: "only a mention, no body",
			in:   `<p>@bot</p>`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mastodonPlainText(tc.in)
			if got != tc.want {
				t.Errorf("mastodonPlainText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMastodonIDGreater pins the id comparison used to advance since_id.
func TestMastodonIDGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"200", "", true},
		{"", "", false},
		{"201", "200", true},
		{"200", "201", false},
		{"1000", "999", true},
		{"999", "1000", false},
		{"200", "200", false},
	}
	for _, tc := range cases {
		if got := mastodonIDGreater(tc.a, tc.b); got != tc.want {
			t.Errorf("mastodonIDGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
