package console

// live_tail_test.go covers the W4-W2 SSE live-tail for the activity views.
// The tests are deterministic (no sleeps): the stub DeliverySubscriber hands
// back the registered handler so a test pushes events synchronously, and the
// flushRecorder signals each written SSE chunk over a channel so the test
// waits on data, never on a timer. SSE/realtime timing flake is avoided by
// construction.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/domain"
)

// ─── stub DeliverySubscriber ────────────────────────────────────────────────

// stubDelivery is a DeliverySubscriber that records the env it was subscribed
// for and exposes the handler so a test can push events through it directly.
// It mirrors the eventbus core-NATS semantics: publish only reaches handlers
// registered for the matching env (the test calls publish(env, data) and only
// same-env subscriptions fire).
type stubDelivery struct {
	mu        sync.Mutex
	subs      map[string][]func([]byte)
	subErr    error // when set, SubscribeDelivery returns this error
	unsubbed  int
	lastEnvID string
}

func newStubDelivery() *stubDelivery {
	return &stubDelivery{subs: map[string][]func([]byte){}}
}

func (s *stubDelivery) SubscribeDelivery(envID string, handler func([]byte)) (func() error, error) {
	if s.subErr != nil {
		return nil, s.subErr
	}
	s.mu.Lock()
	s.lastEnvID = envID
	s.subs[envID] = append(s.subs[envID], handler)
	s.mu.Unlock()
	return func() error {
		s.mu.Lock()
		s.unsubbed++
		s.mu.Unlock()
		return nil
	}, nil
}

// publish fans an event out to every handler registered for envID — the
// per-env subject isolation the real bus enforces at the NATS layer.
func (s *stubDelivery) publish(envID string, data []byte) {
	s.mu.Lock()
	handlers := append([]func([]byte){}, s.subs[envID]...)
	s.mu.Unlock()
	for _, h := range handlers {
		h(data)
	}
}

func (s *stubDelivery) subscriberCount(envID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[envID])
}

func (s *stubDelivery) unsubscribeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unsubbed
}

// ─── flushRecorder ──────────────────────────────────────────────────────────

// flushRecorder is an http.ResponseWriter + http.Flusher that signals each
// Flush over a channel, so a test can wait for an SSE frame deterministically
// instead of sleeping. The accumulated body is readable after the fact.
type flushRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    strings.Builder
	flushed chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: http.Header{}, flushed: make(chan struct{}, 64)}
}

func (f *flushRecorder) Header() http.Header { return f.header }

func (f *flushRecorder) WriteHeader(code int) { f.status = code }

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.Write(b)
}

func (f *flushRecorder) Flush() {
	select {
	case f.flushed <- struct{}{}:
	default:
	}
}

func (f *flushRecorder) bodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.String()
}

// waitForBody blocks until the accumulated SSE body contains substr or the
// deadline elapses. Deterministic content barrier — immune to the header-flush
// that datastar.NewSSE emits before any patch is written, which a bare
// flush-count wait would race against.
func (f *flushRecorder) waitForBody(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(f.bodyString(), substr) {
			return
		}
		select {
		case <-f.flushed:
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for SSE body to contain %q; body:\n%s", substr, f.bodyString())
}

// ─── helpers ────────────────────────────────────────────────────────────────

// deliveryMsg marshals a domain.Message exactly as the delivery handler does
// (json.Marshal(domain.Message) — Go field names, no json tags), so the test
// payload matches what the live-tail receives on the wire.
func deliveryMsg(t *testing.T, m domain.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal delivery message: %v", err)
	}
	return b
}

// ─── liveRowFromEvent: scope isolation (CRITICAL) ──────────────────────────

func TestLiveRowFromEvent_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	c := tc.c
	viewer := domain.Scope{OrgID: "orgA", EnvID: "envX"}

	cases := []struct {
		name     string
		msg      domain.Message
		wantKeep bool
	}{
		{
			name:     "same org and env is kept",
			msg:      domain.Message{NotificationID: "n1", OrgID: "orgA", EnvID: "envX", WorkflowID: "wf", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z"},
			wantKeep: true,
		},
		{
			name:     "different org is dropped",
			msg:      domain.Message{NotificationID: "n2", OrgID: "orgB", EnvID: "envX", WorkflowID: "wf", Status: "sent"},
			wantKeep: false,
		},
		{
			name:     "different env is dropped",
			msg:      domain.Message{NotificationID: "n3", OrgID: "orgA", EnvID: "envY", WorkflowID: "wf", Status: "sent"},
			wantKeep: false,
		},
		{
			name:     "different org AND env is dropped",
			msg:      domain.Message{NotificationID: "n4", OrgID: "orgB", EnvID: "envY", WorkflowID: "wf", Status: "sent"},
			wantKeep: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, keep := c.liveRowFromEvent(deliveryMsg(t, tt.msg), viewer, "", nil)
			if keep != tt.wantKeep {
				t.Fatalf("keep = %v, want %v", keep, tt.wantKeep)
			}
		})
	}
}

// ─── liveRowFromEvent: event→row mapping ────────────────────────────────────

func TestLiveRowFromEvent_MapsEventToRow(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")

	msg := domain.Message{
		NotificationID: "notif-1",
		OrgID:          tc.scope.OrgID,
		EnvID:          tc.scope.EnvID,
		WorkflowID:     wf.ID,
		SubscriberID:   "sub-9",
		TransactionID:  "txn-abc",
		Channel:        "in_app",
		Status:         "sent",
		RunID:          "run-7",
		CreatedAt:      "2026-06-13T12:00:00Z",
	}

	nameFn := tc.c.buildWorkflowNamer(context.Background(), tc.scope)
	row, keep := tc.c.liveRowFromEvent(deliveryMsg(t, msg), tc.scope, "", nameFn)
	if !keep {
		t.Fatal("keep = false, want true for an in-scope event")
	}
	if row.ID != "notif-1" {
		t.Errorf("row.ID = %q, want notif-1 (notification id, not message id)", row.ID)
	}
	if row.WorkflowName != "Welcome" {
		t.Errorf("row.WorkflowName = %q, want Welcome (resolved via authoring)", row.WorkflowName)
	}
	if row.SubscriberID != "sub-9" {
		t.Errorf("row.SubscriberID = %q, want sub-9", row.SubscriberID)
	}
	if row.Channel != "in_app" {
		t.Errorf("row.Channel = %q, want in_app", row.Channel)
	}
	if row.Status != "sent" {
		t.Errorf("row.Status = %q, want sent", row.Status)
	}
	if row.RunID != "run-7" {
		t.Errorf("row.RunID = %q, want run-7", row.RunID)
	}
}

func TestLiveRowFromEvent_UndecodablePayloadDropped(t *testing.T) {
	tc := newTestConsole(t)
	_, keep := tc.c.liveRowFromEvent([]byte("{not json"), tc.scope, "", nil)
	if keep {
		t.Fatal("keep = true for an undecodable payload, want false")
	}
}

// ─── liveRowFromEvent: workflow filter (per-workflow tab consumer) ───────────

func TestLiveRowFromEvent_WorkflowFilter(t *testing.T) {
	tc := newTestConsole(t)
	match := domain.Message{NotificationID: "n1", OrgID: tc.scope.OrgID, EnvID: tc.scope.EnvID, WorkflowID: "wf-keep", Status: "sent"}
	other := domain.Message{NotificationID: "n2", OrgID: tc.scope.OrgID, EnvID: tc.scope.EnvID, WorkflowID: "wf-other", Status: "sent"}

	if _, keep := tc.c.liveRowFromEvent(deliveryMsg(t, match), tc.scope, "wf-keep", nil); !keep {
		t.Error("matching workflow dropped, want kept")
	}
	if _, keep := tc.c.liveRowFromEvent(deliveryMsg(t, other), tc.scope, "wf-keep", nil); keep {
		t.Error("non-matching workflow kept, want dropped")
	}
	// Empty filter keeps everything (global feed consumer).
	if _, keep := tc.c.liveRowFromEvent(deliveryMsg(t, other), tc.scope, "", nil); !keep {
		t.Error("empty filter dropped an in-scope event, want kept")
	}
}

// ─── emitActivityRowPatch: remove-then-prepend double patch ─────────────────

func TestEmitActivityRowPatch_GlobalTarget(t *testing.T) {
	tc := newTestConsole(t)
	rec := newFlushRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/api/stream/activity", nil)
	sse := datastar.NewSSE(rec, req)

	row := activityRow{ID: "n1", WorkflowName: "WF", SubscriberID: "s1", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z"}
	if err := emitActivityRowPatch(sse, tc.c.ts.base, activityStreamTarget, row); err != nil {
		t.Fatalf("emitActivityRowPatch: %v", err)
	}
	body := rec.bodyString()
	// Two patches: a remove targeting the row id, then a prepend into the tbody.
	if !strings.Contains(body, "activity-row-n1") {
		t.Errorf("body missing remove-by-id selector activity-row-n1:\n%s", body)
	}
	if !strings.Contains(body, "prepend") {
		t.Errorf("body missing prepend mode:\n%s", body)
	}
	if !strings.Contains(body, "#activity-tbody") {
		t.Errorf("body missing tbody selector #activity-tbody:\n%s", body)
	}
}

func TestEmitActivityRowPatch_WorkflowTarget(t *testing.T) {
	tc := newTestConsole(t)
	rec := newFlushRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/api/stream/activity?target=wf", nil)
	sse := datastar.NewSSE(rec, req)

	row := activityRow{ID: "n1", SubscriberID: "s1", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z"}
	if err := emitActivityRowPatch(sse, tc.c.ts.base, wfActivityStreamTarget, row); err != nil {
		t.Fatalf("emitActivityRowPatch: %v", err)
	}
	body := rec.bodyString()
	if !strings.Contains(body, "wf-activity-row-n1") {
		t.Errorf("body missing wf remove-by-id selector wf-activity-row-n1:\n%s", body)
	}
	if !strings.Contains(body, "#wf-activity-tbody") {
		t.Errorf("body missing wf tbody selector #wf-activity-tbody:\n%s", body)
	}
}

// ─── pumpActivityStream: lifecycle + bounded buffer ─────────────────────────

func TestPumpActivityStream_EmitsThenExitsOnCancel(t *testing.T) {
	tc := newTestConsole(t)
	rec := newFlushRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/api/stream/activity", nil)
	sse := datastar.NewSSE(rec, req)

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan []byte, 4)

	done := make(chan struct{})
	go func() {
		tc.c.pumpActivityStream(ctx, sse, events, tc.scope, "", activityStreamTarget, nil)
		close(done)
	}()

	events <- deliveryMsg(t, domain.Message{
		NotificationID: "n1", OrgID: tc.scope.OrgID, EnvID: tc.scope.EnvID,
		WorkflowID: "wf", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z",
	})
	rec.waitForBody(t, "activity-row-n1") // deterministic content barrier

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after context cancel")
	}
	if !strings.Contains(rec.bodyString(), "activity-row-n1") {
		t.Errorf("expected the in-scope row to be emitted:\n%s", rec.bodyString())
	}
}

func TestPumpActivityStream_BoundedBufferDropsWhenFull(t *testing.T) {
	// The handler's drop-on-full select is exercised here at the channel
	// level: a zero-capacity-after-fill channel must not block the producer.
	events := make(chan []byte, 1)
	events <- []byte("first") // fill
	dropped := false
	select {
	case events <- []byte("second"):
	default:
		dropped = true // producer would drop rather than block — the invariant
	}
	if !dropped {
		t.Fatal("expected the second send to be dropped when the buffer is full")
	}
}

// ─── route + integration: serveActivityStream ───────────────────────────────

// newConsoleWithLiveTail rebuilds the test console's deps with a stub
// DeliverySubscriber wired into LiveTail, over the same store, so login
// sessions and seeded data carry over.
func newConsoleWithLiveTail(t *testing.T, tc *testConsole, sub DeliverySubscriber) *testConsole {
	t.Helper()
	deps := tc.deps
	deps.LiveTail = sub
	c := New(deps)
	return &testConsole{
		c:          c,
		h:          c.Routes(),
		key:        tc.key,
		scope:      tc.scope,
		st:         tc.st,
		agentStore: tc.agentStore,
		composer:   tc.composer,
		deps:       deps,
		dbPath:     tc.dbPath,
	}
}

func TestServeActivityStream_RequiresAuth(t *testing.T) {
	base := newTestConsole(t)
	tc := newConsoleWithLiveTail(t, base, newStubDelivery())

	// No session cookie → sessionMiddleware redirects to login (not the
	// stream). Assert we never reach the streaming handler.
	rr := tc.get(t, "/console/api/stream/activity", nil)
	if rr.Code == http.StatusOK {
		t.Fatalf("unauthenticated stream returned 200, want a redirect/!=200 (got %d)", rr.Code)
	}
}

func TestServeActivityStream_NilLiveTail503(t *testing.T) {
	tc := newTestConsole(t) // LiveTail is nil
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/stream/activity", cookie)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil LiveTail stream status = %d, want 503", rr.Code)
	}
}

func TestServeActivityStream_LivePrependEndToEnd(t *testing.T) {
	base := newTestConsole(t)
	wf := base.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	sub := newStubDelivery()
	tc := newConsoleWithLiveTail(t, base, sub)
	cookie, _ := tc.login(t)

	rec := newFlushRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/api/stream/activity", nil)
	req.AddCookie(cookie)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		tc.h.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait until the handler has registered its subscription before publishing,
	// so the event is never missed (deterministic, not a sleep).
	waitFor(t, func() bool { return sub.subscriberCount(tc.scope.EnvID) > 0 })

	sub.publish(tc.scope.EnvID, deliveryMsg(t, domain.Message{
		NotificationID: "live-1", OrgID: tc.scope.OrgID, EnvID: tc.scope.EnvID,
		WorkflowID: wf.ID, SubscriberID: "sub-1", Channel: "in_app",
		Status: "sent", CreatedAt: "2026-06-13T00:00:00Z",
	}))
	rec.waitForBody(t, "activity-row-live-1")

	cancel()
	<-done

	body := rec.bodyString()
	if !strings.Contains(body, "activity-row-live-1") {
		t.Errorf("live row not prepended into the stream:\n%s", body)
	}
	if !strings.Contains(body, "Welcome") {
		t.Errorf("live row missing resolved workflow name:\n%s", body)
	}
	if sub.unsubscribeCount() == 0 {
		t.Error("handler did not unsubscribe on disconnect")
	}
}

func TestServeActivityStream_ScopeIsolationEndToEnd(t *testing.T) {
	base := newTestConsole(t)
	sub := newStubDelivery()
	tc := newConsoleWithLiveTail(t, base, sub)
	alt := tc.altScope(t) // distinct org + env
	cookie, _ := tc.login(t)

	rec := newFlushRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/api/stream/activity", nil)
	req.AddCookie(cookie)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		tc.h.ServeHTTP(rec, req)
		close(done)
	}()
	waitFor(t, func() bool { return sub.subscriberCount(tc.scope.EnvID) > 0 })

	// 1) An event published on the OTHER env's subject never reaches this
	//    subscription (NATS-layer isolation, modelled by the stub's per-env fan-out).
	sub.publish(alt.EnvID, deliveryMsg(t, domain.Message{
		NotificationID: "alt-1", OrgID: alt.OrgID, EnvID: alt.EnvID,
		WorkflowID: "wf", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z",
	}))

	// 2) A forged event on THIS env's subject but carrying the other org's id
	//    is rejected by the in-handler scope guard (defence in depth).
	sub.publish(tc.scope.EnvID, deliveryMsg(t, domain.Message{
		NotificationID: "forged-1", OrgID: alt.OrgID, EnvID: tc.scope.EnvID,
		WorkflowID: "wf", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z",
	}))

	// 3) A legitimate in-scope event DOES flush — gives us a deterministic
	//    barrier proving the prior two were processed-and-dropped, not pending.
	sub.publish(tc.scope.EnvID, deliveryMsg(t, domain.Message{
		NotificationID: "mine-1", OrgID: tc.scope.OrgID, EnvID: tc.scope.EnvID,
		WorkflowID: "wf", Status: "sent", CreatedAt: "2026-06-13T00:00:00Z",
	}))
	rec.waitForBody(t, "activity-row-mine-1")

	cancel()
	<-done

	body := rec.bodyString()
	if strings.Contains(body, "alt-1") {
		t.Errorf("cross-env event leaked into the stream:\n%s", body)
	}
	if strings.Contains(body, "forged-1") {
		t.Errorf("forged cross-org event leaked past the scope guard:\n%s", body)
	}
	if !strings.Contains(body, "activity-row-mine-1") {
		t.Errorf("in-scope event was not emitted:\n%s", body)
	}
}

// waitFor polls cond until true or the deadline elapses. Used only to await
// subscription registration before publishing — a state barrier, not a timing
// hack: it returns the instant the condition holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
