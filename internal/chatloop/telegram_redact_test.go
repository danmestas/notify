package chatloop

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// secretToken is a recognizable bot token used across the redaction tests; if it
// ever appears in a returned error or a log line the leak has regressed.
const secretToken = "123456:AAH-SECRET-BOT-TOKEN-do-not-leak"

// syncBuffer is a concurrency-safe bytes.Buffer: the poll goroutine writes log
// lines through the slog handler while the test goroutine polls the contents, so
// both ends must be mutex-guarded (bytes.Buffer is not safe for concurrent use).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTelegramGetUpdates_TransportErrorRedactsToken forces hc.Do to fail with the
// token embedded in the request URL (the *url.Error path that leaks live) and
// asserts the returned error carries the redaction marker, not the token.
func TestTelegramGetUpdates_TransportErrorRedactsToken(t *testing.T) {
	resp := &recordingResponder{}
	core := openTempStore(t)
	svc := New(core, resp, secretToken, "", "support", nil)

	// An unroutable base so hc.Do returns a *url.Error whose URL contains
	// "/bot<secretToken>/getUpdates" — the exact shape that leaked in prod.
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	_, err := svc.telegramGetUpdates(context.Background(), hc, "http://127.0.0.1:1", 0)
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("returned error leaked bot token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "bot<redacted>") {
		t.Fatalf("returned error missing redaction marker: %q", err.Error())
	}
}

// TestRunTelegramPoll_LoggedErrorRedactsToken drives the poll loop through one
// failed getUpdates and asserts the captured WARN log line contains neither the
// token nor any substring of it, but does carry the redaction marker.
func TestRunTelegramPoll_LoggedErrorRedactsToken(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resp := &recordingResponder{}
	core := openTempStore(t)
	svc := New(core, resp, secretToken, "", "support", logger).
		withTelegramBaseURL("http://127.0.0.1:1").
		withHTTPClient(&http.Client{Timeout: 300 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.runTelegramPoll(ctx, domain.Scope{OrgID: "o1", EnvID: "e1"})
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for !strings.Contains(buf.String(), "getUpdates") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out waiting for getUpdates warning; log so far: %q", buf.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	got := buf.String()
	if strings.Contains(got, secretToken) {
		t.Fatalf("log leaked bot token: %q", got)
	}
	// Also assert the raw token (before the colon) does not leak as a substring.
	if strings.Contains(got, "AAH-SECRET-BOT-TOKEN") {
		t.Fatalf("log leaked token substring: %q", got)
	}
	if !strings.Contains(got, "bot<redacted>") {
		t.Fatalf("log missing redaction marker: %q", got)
	}
}

// TestTelegramSender_TransportErrorRedactsToken covers the reply path: the
// sendMessage URL also embeds bot<token>, so a transport fault wraps a *url.Error
// that must be redacted before it is returned (and logged by ReplyHook).
func TestTelegramSender_TransportErrorRedactsToken(t *testing.T) {
	s := newTelegramSender(secretToken, "http://127.0.0.1:1", &http.Client{Timeout: 300 * time.Millisecond})
	err := s.send(context.Background(), "555", "hi")
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("sender error leaked bot token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "bot<redacted>") {
		t.Fatalf("sender error missing redaction marker: %q", err.Error())
	}
}
