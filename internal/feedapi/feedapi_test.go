package feedapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
)

// scopeInjector mimics identity.Middleware by stamping a fixed Scope onto the
// request context so the gateway sees a tenant, mirroring
// internal/realtime/realtime_test.go's helper of the same name.
func scopeInjector(s domain.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(domain.ContextWithScope(r.Context(), s)))
	})
}

// sseFrame is one parsed "id: <n>\ndata: <payload>\n\n" frame.
type sseFrame struct {
	id   string
	data string
}

// sseStream wraps an open SSE connection with a single scanner goroutine
// feeding a channel, so tests can read frames incrementally (backlog, then
// live) without racing two scanners over the same response body.
type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
	srv    *httptest.Server
}

// read blocks for n frames (skipping ": ping" comment lines, which never
// produce a frame) or fails the test after timeout.
func (s *sseStream) read(t *testing.T, n int, timeout time.Duration) []sseFrame {
	t.Helper()

	var got []sseFrame
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatalf("stream closed after %d/%d frames: %v", len(got), n, got)
			}
			got = append(got, f)
		case <-deadline:
			t.Fatalf("timed out waiting for %d SSE frames, got %d: %v", n, len(got), got)
		}
	}
	return got
}

// expectNoMore asserts no further frame arrives within d.
func (s *sseStream) expectNoMore(t *testing.T, d time.Duration) {
	t.Helper()

	select {
	case f, ok := <-s.frames:
		if ok {
			t.Fatalf("received unexpected extra frame: %+v", f)
		}
	case <-time.After(d):
	}
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
	s.srv.Close()
}

func connectFeedGateway(t *testing.T) (*Gateway, *eventbus.Bus) {
	t.Helper()
	natsURL := dagtest.StartSidecar(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	return New(bus), bus
}

// startStream opens a scoped SSE connection to the gateway and returns an
// sseStream reading frames off it. Callers must call stream.close() (or rely
// on it via a deferred call) to tear down the request, connection, and test
// server.
func startStream(t *testing.T, g *Gateway, scope domain.Scope, query string, header http.Header) *sseStream {
	t.Helper()

	r := chi.NewRouter()
	g.Routes(r)
	handler := scopeInjector(scope, r)
	srv := httptest.NewServer(handler)

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/v1/feed/stream?"+query, nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("NewRequest: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by sseStream.close()
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		srv.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := make(chan sseFrame, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if cur.data != "" {
					frames <- cur
					cur = sseFrame{}
				}
			}
		}
	}()

	return &sseStream{resp: resp, frames: frames, cancel: cancel, srv: srv}
}

// TestFeedStream_Backlog_ThenLive publishes e1,e2 to env1/orders, connects
// with from=0 (i.e. "nothing seen yet" -> resumes at the first ever stream
// sequence, inclusive), reads both as backlog frames, then publishes e3
// while connected and asserts it arrives live too.
func TestFeedStream_Backlog_ThenLive(t *testing.T) {
	g, bus := connectFeedGateway(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("PublishFeed(1): %v", err)
	}
	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("PublishFeed(2): %v", err)
	}

	stream := startStream(t, g, scope, "topic=orders&from=0", nil)
	defer stream.close()

	got := stream.read(t, 2, 5*time.Second)
	if got[0].data != `{"n":1}` || got[1].data != `{"n":2}` {
		t.Fatalf("backlog frames = %+v, want n:1 then n:2", got)
	}
	if got[0].id == "" || got[1].id == "" || got[0].id == got[1].id {
		t.Fatalf("backlog frame ids = %q, %q, want distinct non-empty stream seqs", got[0].id, got[1].id)
	}

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":3}`)); err != nil {
		t.Fatalf("PublishFeed(3): %v", err)
	}
	live := stream.read(t, 1, 5*time.Second)
	if live[0].data != `{"n":3}` {
		t.Fatalf("live frame = %+v, want n:3", live[0])
	}
}

// TestFeedStream_LastEventIDResume connects, reads e1,e2, disconnects, then
// reconnects with Last-Event-ID set to e2's seq and asserts only e3 (no
// duplicates of e1/e2) arrives.
func TestFeedStream_LastEventIDResume(t *testing.T) {
	g, bus := connectFeedGateway(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("PublishFeed(1): %v", err)
	}
	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("PublishFeed(2): %v", err)
	}

	stream := startStream(t, g, scope, "topic=orders&from=0", nil)
	got := stream.read(t, 2, 5*time.Second)
	stream.close()

	lastID := got[1].id // seq of e2

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":3}`)); err != nil {
		t.Fatalf("PublishFeed(3): %v", err)
	}

	stream2 := startStream(t, g, scope, "topic=orders", http.Header{"Last-Event-ID": []string{lastID}})
	defer stream2.close()

	resumed := stream2.read(t, 1, 5*time.Second)
	if resumed[0].data != `{"n":3}` {
		t.Fatalf("resumed frame = %+v, want n:3 only", resumed[0])
	}
	stream2.expectNoMore(t, 500*time.Millisecond)
}

// TestFeedStream_FromTime asserts from=<RFC3339> between e1 and e2 delivers
// e2 onward only.
func TestFeedStream_FromTime(t *testing.T) {
	g, bus := connectFeedGateway(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("PublishFeed(1): %v", err)
	}
	// Give the stream a moment so the cutoff strictly separates e1 from e2,
	// mirroring internal/eventbus/feed_test.go's TestConsumeFeed_FromTime.
	time.Sleep(1100 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(1100 * time.Millisecond)

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("PublishFeed(2): %v", err)
	}

	stream := startStream(t, g, scope, "topic=orders&from="+cutoff.Format(time.RFC3339), nil)
	defer stream.close()

	got := stream.read(t, 1, 5*time.Second)
	if got[0].data != `{"n":2}` {
		t.Fatalf("frame = %+v, want n:2 only", got[0])
	}
	stream.expectNoMore(t, 500*time.Millisecond)
}

// TestFeedStream_TenantIsolation publishes to envA and envB on the same
// topic and asserts a client scoped to envA receives only envA frames — the
// multi-tenant HTTP proof.
func TestFeedStream_TenantIsolation(t *testing.T) {
	g, bus := connectFeedGateway(t)

	if err := bus.PublishFeed("envB", "orders", []byte(`{"who":"B"}`)); err != nil {
		t.Fatalf("PublishFeed(envB): %v", err)
	}
	if err := bus.PublishFeed("envA", "orders", []byte(`{"who":"A"}`)); err != nil {
		t.Fatalf("PublishFeed(envA): %v", err)
	}

	stream := startStream(t, g, domain.Scope{OrgID: "org1", EnvID: "envA"}, "topic=orders&from=0", nil)
	defer stream.close()

	got := stream.read(t, 1, 5*time.Second)
	if got[0].data != `{"who":"A"}` {
		t.Fatalf("frame = %+v, want only envA's data", got[0])
	}
	stream.expectNoMore(t, 500*time.Millisecond)
}

// TestFeedStream_BadInputs asserts the edge-validation gates: missing/
// wildcard/malformed topic and malformed from all 400; missing scope 401.
func TestFeedStream_BadInputs(t *testing.T) {
	g := New(nil)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	cases := []struct {
		name   string
		scoped bool
		query  string
		want   int
	}{
		{"missing topic", true, "", http.StatusBadRequest},
		{"wildcard topic", true, "topic=orders.*", http.StatusBadRequest},
		{"bare wildcard topic", true, "topic=*", http.StatusBadRequest},
		{"empty segment topic", true, "topic=a..b", http.StatusBadRequest},
		{"malformed from", true, "topic=orders&from=not-a-seq-or-time", http.StatusBadRequest},
		{"no scope", false, "topic=orders", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			g.Routes(r)
			var handler http.Handler = r
			if tc.scoped {
				handler = scopeInjector(scope, r)
			}

			req := httptest.NewRequest(http.MethodGet, "/v1/feed/stream?"+tc.query, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestFeedStream_LiveOnlyWhenNoFrom asserts that without from= or
// Last-Event-ID, only events published after connecting are delivered.
func TestFeedStream_LiveOnlyWhenNoFrom(t *testing.T) {
	g, bus := connectFeedGateway(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("PublishFeed(1): %v", err)
	}

	stream := startStream(t, g, scope, "topic=orders", nil)
	defer stream.close()

	// Give ConsumeFeed's live-only consumer a moment to establish before
	// publishing, avoiding a race with subscription setup.
	time.Sleep(300 * time.Millisecond)

	if err := bus.PublishFeed("env1", "orders", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("PublishFeed(2): %v", err)
	}

	got := stream.read(t, 1, 5*time.Second)
	if got[0].data != `{"n":2}` {
		t.Fatalf("frame = %+v, want n:2 only (n:1 predates the connection)", got[0])
	}
}

// TestFeedStream_NoScope_401 mirrors realtime_test.go's coverage of the
// unscoped-request 401 path directly (in addition to the table-driven case
// in TestFeedStream_BadInputs) since it is the auth precondition every other
// test in this file depends on.
func TestFeedStream_NoScope_401(t *testing.T) {
	g := New(nil)
	r := chi.NewRouter()
	g.Routes(r)

	req := httptest.NewRequest(http.MethodGet, "/v1/feed/stream?topic=orders", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
