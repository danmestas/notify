package webhooksout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// TestConsumer_AtLeastOnce_KillAndRestartMidStream is the Shape-B headline
// proof: an event published while the webhooksout consumer is down (e.g. a
// notify restart) is still delivered once a consumer resumes, because Start
// now rides the durable JetStream consumer from B1 instead of an ephemeral
// core-NATS subscription.
//
// With the OLD core.Subscribe-based Start this test would fail: e2, published
// after stop(), would never reach a fresh subscription — core NATS has no
// replay/catch-up for messages published while nobody was subscribed.
func TestConsumer_AtLeastOnce_KillAndRestartMidStream(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	received := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    srv.URL,
		Active: true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// consumer1 stands in for the notify process before the crash.
	consumer1 := NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second})
	stop1, err := consumer1.Start(ctx)
	if err != nil {
		t.Fatalf("Start (1): %v", err)
	}

	msg1 := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "e1"}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg1)); err != nil {
		t.Fatalf("PublishDelivery(e1): %v", err)
	}

	select {
	case body := <-received:
		var got domain.Message
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal e1: %v", err)
		}
		if got.Status != "e1" {
			t.Fatalf("expected e1, got %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for e1 POST")
	}

	// Simulate a notify crash/restart: stop the consumer entirely.
	if err := stop1(); err != nil {
		t.Fatalf("stop1: %v", err)
	}
	// Give e1's ack time to reach the server before publishing e2, so the
	// durable consumer's last-acked position is settled before restart.
	time.Sleep(300 * time.Millisecond)

	// Publish e2 WHILE the consumer is down — nobody is subscribed/consuming.
	msg2 := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "e2"}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg2)); err != nil {
		t.Fatalf("PublishDelivery(e2): %v", err)
	}

	// consumer2 is a BRAND NEW Consumer (what a fresh notify process
	// constructs on restart), started against the same durable name under
	// the hood.
	consumer2 := NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second})
	stop2, err := consumer2.Start(ctx)
	if err != nil {
		t.Fatalf("Start (2): %v", err)
	}
	defer func() { _ = stop2() }()

	// Delivery is at-least-once: if stop1 cancelled e1's POST after the
	// server read it but before the response came back, e1 is released and
	// the new consumer sends it again. Receivers dedupe on the message id, so
	// a repeat of e1 is allowed here. What must not happen is losing e2.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case body := <-received:
			var got domain.Message
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal after restart: %v", err)
			}
			switch got.Status {
			case "e2":
				return
			case "e1":
				continue
			default:
				t.Fatalf("unexpected event after restart: %+v", got)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for e2 POST after restart: event published during downtime was lost")
		}
	}
}

// TestConsumer_PoisonEventDoesNotWedge asserts a malformed (non-JSON) event
// on the stream is acked rather than endlessly redelivered, and does not
// block a subsequent valid event from being processed.
func TestConsumer_PoisonEventDoesNotWedge(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	received := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    srv.URL,
		Active: true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	consumer := NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second})
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	// Poison event: not valid JSON at all.
	if err := bus.PublishDelivery(scope.EnvID, []byte("this is not json")); err != nil {
		t.Fatalf("PublishDelivery(poison): %v", err)
	}

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "valid"}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery(valid): %v", err)
	}

	select {
	case body := <-received:
		var got domain.Message
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Status != "valid" {
			t.Fatalf("expected the valid event to reach the endpoint, got %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out: poison event appears to have wedged the durable consumer")
	}

	// Nothing else should arrive: the poison event was acked, not
	// redelivered forever.
	select {
	case extra := <-received:
		t.Fatalf("expected exactly one POST, got extra: %s", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// flakyLister is an endpointLister stub that returns a transient error for the
// first failFirst calls, then serves endpoints. It stands in for a store under
// DB-busy / lock contention that clears on retry.
type flakyLister struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	endpoints []domain.WebhookEndpoint
}

func (f *flakyLister) ListActiveByEnv(_ context.Context, _ domain.Scope) ([]domain.WebhookEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failFirst {
		return nil, fmt.Errorf("simulated transient store error (call %d)", f.calls)
	}
	return f.endpoints, nil
}

func (f *flakyLister) GetByID(_ context.Context, _ domain.Scope, id string) (domain.WebhookEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.endpoints {
		if e.ID == id {
			return e, nil
		}
	}
	return domain.WebhookEndpoint{}, fmt.Errorf("no endpoint %q", id)
}

func (f *flakyLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestConsumer_StoreErrorNaksAndRedelivers is the fix for the blocking review
// finding: a transient store-lookup error must NAK (not ack-and-drop), so
// JetStream redelivers the event (bounded by MaxDeliver:5) and it eventually
// reaches the endpoint once the store recovers. The old code acked
// immediately on a store error, permanently losing the event on a single
// transient DB-busy hiccup.
func TestConsumer_StoreErrorNaksAndRedelivers(t *testing.T) {
	bus := newBus(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	received := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Fail the first two lookups (nak → redeliver), succeed on the third.
	// Well within MaxDeliver:5.
	lister := &flakyLister{
		failFirst: 2,
		endpoints: []domain.WebhookEndpoint{{ID: "ep1", URL: srv.URL, Active: true}},
	}
	consumer := NewWithClient(bus, newStore(t), &http.Client{Timeout: 5 * time.Second})
	consumer.endpoints = lister
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "survives"}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	select {
	case body := <-received:
		var got domain.Message
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Status != "survives" {
			t.Fatalf("expected the event to survive transient store errors, got %+v", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("timed out: event was dropped on a transient store error instead of being redelivered")
	}

	// It must have taken more than one lookup: the nak/redeliver path was
	// genuinely exercised (not an accidental first-try success).
	if n := lister.callCount(); n <= lister.failFirst {
		t.Fatalf("lookup calls = %d, want > %d (redelivery should have retried the store)", n, lister.failFirst)
	}
}
