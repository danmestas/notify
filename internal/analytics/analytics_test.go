package analytics

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

var testScope = domain.Scope{OrgID: "o1", EnvID: "e1"}

func newBus(t *testing.T) *eventbus.Bus {
	t.Helper()
	natsURL := dagtest.StartSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func event(t *testing.T, id, status string) []byte {
	t.Helper()
	b, err := json.Marshal(domain.Message{ID: id, OrgID: testScope.OrgID, EnvID: testScope.EnvID, Channel: "email", Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func total(t *testing.T, svc *Service) int {
	t.Helper()
	m, err := svc.DeliveriesByChannel(context.Background(), testScope, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("DeliveriesByChannel: %v", err)
	}
	n := 0
	for _, byStatus := range m {
		for _, c := range byStatus {
			n += c
		}
	}
	return n
}

func waitTotal(t *testing.T, svc *Service, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if total(t, svc) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("analytics total = %d, want %d", total(t, svc), want)
}

// settle gives a wrongly redelivered or double-counted event time to show up.
func settle() { time.Sleep(500 * time.Millisecond) }

func TestProjector_ResumesAfterDowntime(t *testing.T) {
	bus, st := newBus(t), newStore(t)
	ctx := context.Background()
	svc := New(st, bus)

	stop, err := svc.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := bus.PublishDelivery("e1", event(t, "m1", "sent")); err != nil {
		t.Fatal(err)
	}
	waitTotal(t, svc, 1)
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Published while the projector is down.
	for _, id := range []string{"m2", "m3"} {
		if err := bus.PublishDelivery("e1", event(t, id, "sent")); err != nil {
			t.Fatal(err)
		}
	}

	stop2, err := New(st, bus).Start(ctx)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { _ = stop2() }()
	waitTotal(t, svc, 3)
	settle()
	if got := total(t, svc); got != 3 {
		t.Fatalf("after restart total = %d, want 3 (no double count of m1)", got)
	}
}

func TestProjector_DuplicateEventCountedOnce(t *testing.T) {
	bus, st := newBus(t), newStore(t)
	svc := New(st, bus)
	stop, err := svc.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop() }()

	// Redelivery: the same event reaching ingest twice (bypasses the
	// publish-side Nats-Msg-Id dedupe).
	ev := event(t, "m1", "sent")
	for i := 0; i < 2; i++ {
		if err := svc.ingest(context.Background(), ev); err != nil {
			t.Fatalf("ingest #%d: %v", i, err)
		}
	}
	// And the same event published twice over the bus.
	for i := 0; i < 2; i++ {
		if err := bus.PublishDelivery("e1", event(t, "m2", "sent")); err != nil {
			t.Fatal(err)
		}
	}
	waitTotal(t, svc, 2)
	settle()
	if got := total(t, svc); got != 2 {
		t.Fatalf("total = %d, want 2", got)
	}
}

func TestIngest_MalformedAndUnscopedAreAcked(t *testing.T) {
	svc := New(newStore(t), nil)
	ctx := context.Background()
	if err := svc.ingest(ctx, []byte("not json")); err != nil {
		t.Fatalf("malformed: err = %v, want nil (ack and drop)", err)
	}
	if err := svc.ingest(ctx, []byte(`{"ID":"m1","Status":"sent"}`)); err != nil {
		t.Fatalf("unscoped: err = %v, want nil (ack and drop)", err)
	}
	if got := total(t, svc); got != 0 {
		t.Fatalf("total = %d, want 0", got)
	}
}

func TestIngest_StoreErrorIsRetried(t *testing.T) {
	st := newStore(t)
	svc := New(st, nil)
	_ = st.Close()
	if err := svc.ingest(context.Background(), event(t, "m1", "sent")); err == nil {
		t.Fatal("ingest on a closed store = nil, want error so the message is Nak'd")
	}
}
