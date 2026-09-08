package engine

import (
	"bytes"
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
)

// newTestEngine builds an Engine pointed at srv with no NATS connection. Every
// path exercised here is HTTP-only, so nc stays nil.
func newTestEngine(bridgeURL string, opts ...Option) *Engine {
	e := &Engine{
		bridgeURL:     bridgeURL,
		http:          srvClient(),
		log:           slog.Default(),
		handlers:      make(map[string]HandlerFunc),
		agentHandlers: make(map[string]AgentLoopFunc),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func srvClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// TestBridgeToken_SentOnPollAndResolve locks the fix for #325: without this
// header a token-gated dagnats bridge answers 401 to every poll and no
// workflow step ever executes.
func TestBridgeToken_SentOnPollAndResolve(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "token set sends bearer", token: "s3cret", want: "Bearer s3cret"},
		{name: "no token sends no header", token: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPoll, gotResolve string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/poll"):
					gotPoll = r.Header.Get("Authorization")
					_, _ = w.Write([]byte(`[]`))
				default:
					gotResolve = r.Header.Get("Authorization")
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()

			var opts []Option
			if tc.token != "" {
				opts = append(opts, WithBridgeToken(tc.token))
			}
			e := newTestEngine(srv.URL, opts...)

			if _, err := e.poll(context.Background(), "send.in_app", 10); err != nil {
				t.Fatalf("poll: %v", err)
			}
			e.runTask(context.Background(),
				func(context.Context, TaskPayload) (json.RawMessage, error) { return nil, nil },
				TaskPayload{TaskID: "t1"})

			if gotPoll != tc.want {
				t.Errorf("poll Authorization = %q, want %q", gotPoll, tc.want)
			}
			if gotResolve != tc.want {
				t.Errorf("resolve Authorization = %q, want %q", gotResolve, tc.want)
			}
		})
	}
}

// TestBridgeToken_NeverInError guards against the secret leaking into an error
// string that will be logged.
func TestBridgeToken_NeverInError(t *testing.T) {
	const token = "super-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	e := newTestEngine(srv.URL, WithBridgeToken(token))
	_, err := e.poll(context.Background(), "send.in_app", 10)
	if err == nil {
		t.Fatal("poll succeeded against a 401 bridge, want error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("bridge token leaked into error: %v", err)
	}
}

// TestPollLoop_LogsFailures is the mutation guard for the silent-401 half of
// #325: delete the warn in pollLoop and this test fails.
func TestPollLoop_LogsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	var buf lockedBuffer
	e := newTestEngine(srv.URL, WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))))

	ctx, cancel := context.WithCancel(context.Background())
	e.wg.Add(1)
	go e.pollLoop(ctx, "send.in_app")
	waitFor(t, func() bool { return strings.Contains(buf.String(), "send.in_app") })
	cancel()
	e.wg.Wait()

	out := buf.String()
	if !strings.Contains(out, "bridge poll failed") {
		t.Fatalf("no poll-failure warning logged; a permanently rejected poll must not be silent.\ngot: %s", out)
	}
	if !strings.Contains(out, "send.in_app") {
		t.Errorf("warning does not name the task type: %s", out)
	}
}

// TestPollLoop_RateLimitsAndResets asserts the two halves of the reporting
// policy: a persistent fault does not flood, and a fault that returns after a
// success is reported immediately rather than waiting out a stale timer.
func TestPollLoop_RateLimitsAndResets(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		if failing.Load() {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	var buf lockedBuffer
	e := newTestEngine(srv.URL, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

	ctx, cancel := context.WithCancel(context.Background())
	e.wg.Add(1)
	go e.pollLoop(ctx, "send.in_app")

	// Many consecutive failures, far more than one report's worth.
	waitFor(t, func() bool { return polls.Load() >= 10 })
	if got := strings.Count(buf.String(), "bridge poll failed"); got != 1 {
		t.Fatalf("warnings after %d failed polls = %d, want exactly 1 (rate limiting)", polls.Load(), got)
	}

	// Recover, then fail again: the new fault must report at once.
	failing.Store(false)
	waitFor(t, func() bool { return strings.Contains(buf.String(), "bridge poll recovered") })
	failing.Store(true)
	waitFor(t, func() bool { return strings.Count(buf.String(), "bridge poll failed") == 2 })

	cancel()
	e.wg.Wait()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 10s")
}

// lockedBuffer is a race-free io.Writer for slog output under -race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
