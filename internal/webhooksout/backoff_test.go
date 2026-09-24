package webhooksout

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// countingServer records POSTs by the event's Status and answers with the
// configured code, which the test may flip at runtime.
type countingServer struct {
	*httptest.Server
	mu    sync.Mutex
	seen  map[string]int
	ok    map[string]int
	code  int
	times map[string][]time.Time
}

func newCountingServer(t *testing.T, code int) *countingServer {
	t.Helper()
	cs := &countingServer{seen: map[string]int{}, ok: map[string]int{}, times: map[string][]time.Time{}, code: code}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got domain.Message
		_ = json.Unmarshal(body, &got)
		cs.mu.Lock()
		cs.seen[got.Status]++
		cs.times[got.Status] = append(cs.times[got.Status], time.Now())
		code := cs.code
		if code >= 200 && code < 300 {
			cs.ok[got.Status]++
		}
		cs.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *countingServer) count(status string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.seen[status]
}

// okCount is how many POSTs for status the server accepted with a 2xx.
func (cs *countingServer) okCount(status string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.ok[status]
}

func (cs *countingServer) setCode(code int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.code = code
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fastConsumer is a consumer whose minutes are milliseconds.
func fastConsumer(t *testing.T, st *store.Store) *Consumer {
	t.Helper()
	c := NewWithClient(newBus(t), st, &http.Client{Timeout: 5 * time.Second})
	c.backoff = func(attempt int) time.Duration { return 30 * time.Millisecond }
	c.tick = 10 * time.Millisecond
	c.maxAttempts = 4
	return c
}

func pending(t *testing.T, st *store.Store) int {
	t.Helper()
	rows, err := st.WebhookDeliveries().Due(context.Background(), "9999", 1000)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	return len(rows)
}

// TestConsumer_DeadEndpointBacksOffPerEndpoint: with one endpoint down and
// one up in the same scope, the healthy one receives the event once and
// promptly, the dead one is retried on its own clock, and neither retry
// touches the healthy endpoint again.
func TestConsumer_DeadEndpointBacksOffPerEndpoint(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	dead := newCountingServer(t, http.StatusInternalServerError)
	live := newCountingServer(t, http.StatusOK)
	for _, u := range []string{dead.URL, live.URL} {
		if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: u, Active: true}); err != nil {
			t.Fatalf("create endpoint: %v", err)
		}
	}
	c := fastConsumer(t, st)
	stop, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "ev"}
	if err := c.bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	waitFor(t, "the live endpoint to receive the event", 3*time.Second, func() bool { return live.count("ev") == 1 })
	waitFor(t, "the dead endpoint to be retried", 3*time.Second, func() bool { return dead.count("ev") >= 3 })
	if got := live.count("ev"); got != 1 {
		t.Fatalf("live endpoint received %d POSTs, want exactly 1: a dead sibling must not cause re-sends", got)
	}
	// Retries are spaced by the backoff, not fired back to back.
	dead.mu.Lock()
	ts := dead.times["ev"]
	dead.mu.Unlock()
	if gap := ts[2].Sub(ts[1]); gap < 25*time.Millisecond {
		t.Fatalf("retry gap %v, want >= the 30ms backoff", gap)
	}
}

// TestConsumer_RecoveredEndpointGetsTheEvent: an endpoint that comes back
// within the retry window receives the event, and the outbox row is gone.
func TestConsumer_RecoveredEndpointGetsTheEvent(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	srv := newCountingServer(t, http.StatusBadGateway)
	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: srv.URL, Active: true}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	c := fastConsumer(t, st)
	// Enough attempts that the row cannot run out before the endpoint
	// recovers, which would pass the drain check without a delivery.
	c.maxAttempts = 1000
	stop, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "ev"}
	if err := c.bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	waitFor(t, "first failed attempt", 3*time.Second, func() bool { return srv.count("ev") >= 1 })
	srv.setCode(http.StatusOK)
	waitFor(t, "the outbox to drain", 3*time.Second, func() bool { return pending(t, st) == 0 })
	if got := srv.okCount("ev"); got != 1 {
		t.Fatalf("endpoint accepted %d POSTs after recovery, want exactly 1", got)
	}
}

// TestConsumer_RetriesSurviveRestart: a pending delivery outlives the
// consumer that enqueued it. A fresh consumer over the same store finishes
// the job with no new event published.
func TestConsumer_RetriesSurviveRestart(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	srv := newCountingServer(t, http.StatusServiceUnavailable)
	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: srv.URL, Active: true}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	first := fastConsumer(t, st)
	// The row must still exist when the consumer stops: with the default
	// four attempts 30ms apart, a loaded machine can exhaust them between
	// the first failure and stop(), and the row is dropped.
	first.maxAttempts = 1000
	stop, err := first.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "ev"}
	if err := first.bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	waitFor(t, "first failed attempt", 3*time.Second, func() bool { return srv.count("ev") >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if pending(t, st) != 1 {
		t.Fatalf("pending after stop = %d, want 1", pending(t, st))
	}

	srv.setCode(http.StatusOK)
	second := fastConsumer(t, st)
	second.maxAttempts = 1000
	stop2, err := second.Start(ctx)
	if err != nil {
		t.Fatalf("Start(second): %v", err)
	}
	defer func() { _ = stop2() }()
	waitFor(t, "the restarted worker to deliver", 3*time.Second, func() bool { return pending(t, st) == 0 })
	if srv.count("ev") < 2 {
		t.Fatal("the restarted consumer never delivered the pending row")
	}
}

// TestConsumer_GivesUpAfterMaxAttempts: a permanently dead endpoint is
// tried maxAttempts times and then its row is dropped.
func TestConsumer_GivesUpAfterMaxAttempts(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	srv := newCountingServer(t, http.StatusInternalServerError)
	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: srv.URL, Active: true}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	c := fastConsumer(t, st)
	stop, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "ev"}
	if err := c.bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	waitFor(t, "the row to be dropped", 3*time.Second, func() bool { return pending(t, st) == 0 && srv.count("ev") >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := srv.count("ev"); got != c.maxAttempts {
		t.Fatalf("dead endpoint saw %d POSTs, want exactly maxAttempts=%d", got, c.maxAttempts)
	}
}

// TestConsumer_DisabledEndpointDropsPending: switching an endpoint off
// discards what was queued for it instead of retrying into the void.
func TestConsumer_DisabledEndpointDropsPending(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	srv := newCountingServer(t, http.StatusInternalServerError)
	ep, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: srv.URL, Active: true})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	c := fastConsumer(t, st)
	c.maxAttempts = 100
	stop, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "ev"}
	if err := c.bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	waitFor(t, "first failed attempt", 3*time.Second, func() bool { return srv.count("ev") >= 1 })
	if _, err := st.WebhookEndpoints().SetActive(ctx, scope, ep.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	waitFor(t, "the row to be dropped", 3*time.Second, func() bool { return pending(t, st) == 0 })
	n := srv.count("ev")
	time.Sleep(100 * time.Millisecond)
	if srv.count("ev") != n {
		t.Fatal("a disabled endpoint was still being retried")
	}
}

// TestDefaultBackoff pins the production schedule: 5s doubling to a 30m cap.
func TestDefaultBackoff(t *testing.T) {
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 320 * time.Second, 640 * time.Second, 1280 * time.Second, 30 * time.Minute, 30 * time.Minute,
	}
	for i, w := range want {
		if got := defaultBackoff(i + 1); got != w {
			t.Errorf("defaultBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// TestConsumer_ShutdownMidAttemptReleasesTheRow: stopping the consumer while
// a POST is in flight must hand the row straight back, not leave it leased
// for a minute where a restarted worker cannot reach it, and must not count
// the interrupted POST as a failed attempt.
func TestConsumer_ShutdownMidAttemptReleasesTheRow(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	inFlight := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case inFlight <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { close(release); slow.Close() })
	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: slow.URL, Active: true}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	c := fastConsumer(t, st)
	stop, err := c.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "ev"}
	if err := c.bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	select {
	case <-inFlight:
	case <-time.After(3 * time.Second):
		t.Fatal("POST never started")
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	rows, err := st.WebhookDeliveries().Due(ctx, timestamp.Now(), 10)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("due rows right after shutdown = %d, want 1: the interrupted row is still leased", len(rows))
	}
	if rows[0].Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: shutdown is not the endpoint's failure", rows[0].Attempts)
	}
}
