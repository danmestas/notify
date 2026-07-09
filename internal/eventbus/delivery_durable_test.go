package eventbus

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// TestConnect_EnsuresDeliveryStream asserts Connect idempotently creates the
// NOTIFY_DELIVERY stream with the expected subjects and retention, verified
// from a raw JetStream client independent of the Bus's private handles.
func TestConnect_EnsuresDeliveryStream(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus1, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	defer bus1.Close()

	bus2, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("second Connect (idempotent): %v", err)
	}
	defer bus2.Close()

	js := inspectJS(t, natsURL)
	stream, err := js.Stream(ctx, deliveryStream)
	if err != nil {
		t.Fatalf("Stream(%q): %v", deliveryStream, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if !slices.Contains(info.Config.Subjects, deliverySubjectAll) {
		t.Fatalf("stream %q subjects = %v, want to contain %q",
			deliveryStream, info.Config.Subjects, deliverySubjectAll)
	}
	if info.Config.MaxAge != deliveryStreamMaxAge {
		t.Fatalf("stream %q MaxAge = %v, want %v", deliveryStream, info.Config.MaxAge, deliveryStreamMaxAge)
	}
}

// TestConsumeAllDeliveryDurable_ReceivesAndAcks publishes three delivery
// events and asserts a durable consumer receives each exactly once.
func TestConsumeAllDeliveryDurable_ReceivesAndAcks(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	for i := range 3 {
		if err := bus.PublishDelivery("env1", []byte(fmt.Sprintf(`{"event":"e%d"}`, i))); err != nil {
			t.Fatalf("PublishDelivery(e%d): %v", i, err)
		}
	}

	got := make(chan []byte, 10)
	stop, err := bus.ConsumeAllDeliveryDurable(ctx, "wh-receive", func(data []byte) error {
		got <- data
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeAllDeliveryDurable: %v", err)
	}
	defer func() { _ = stop() }()

	seen := map[string]int{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 3 {
		select {
		case data := <-got:
			seen[string(data)]++
		case <-deadline:
			t.Fatalf("timed out; only received %v", seen)
		}
	}
	for k, v := range seen {
		if v != 1 {
			t.Fatalf("event %q delivered %d times, want exactly once", k, v)
		}
	}
}

// TestConsumeAllDeliveryDurable_SurvivesRestart is the durability proof: a
// durable consumer that stops (e.g. webhooksout restarts) and later resumes
// with the SAME durable name must catch up on everything published while it
// was down, and must NOT redeliver what it already acked.
func TestConsumeAllDeliveryDurable_SurvivesRestart(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	if err := bus.PublishDelivery("env1", []byte(`{"event":"e1"}`)); err != nil {
		t.Fatalf("PublishDelivery(e1): %v", err)
	}

	got := make(chan []byte, 10)
	stop, err := bus.ConsumeAllDeliveryDurable(ctx, "wh-restart", func(data []byte) error {
		got <- data
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeAllDeliveryDurable: %v", err)
	}

	select {
	case data := <-got:
		if string(data) != `{"event":"e1"}` {
			t.Fatalf("payload = %q, want e1", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for e1")
	}

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Give the ack for e1 time to reach the server before publishing more
	// and restarting the consumer, so the restart assertion below is not
	// racing the (fire-and-forget) ack flush.
	time.Sleep(500 * time.Millisecond)

	// Publish while NO consumer is running.
	if err := bus.PublishDelivery("env1", []byte(`{"event":"e2"}`)); err != nil {
		t.Fatalf("PublishDelivery(e2): %v", err)
	}
	if err := bus.PublishDelivery("env1", []byte(`{"event":"e3"}`)); err != nil {
		t.Fatalf("PublishDelivery(e3): %v", err)
	}

	// Restart with the SAME durable name: must catch up on e2 and e3.
	stop2, err := bus.ConsumeAllDeliveryDurable(ctx, "wh-restart", func(data []byte) error {
		got <- data
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeAllDeliveryDurable (restart): %v", err)
	}
	defer func() { _ = stop2() }()

	seen := map[string]int{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case data := <-got:
			seen[string(data)]++
		case <-deadline:
			t.Fatalf("timed out; only received %v", seen)
		}
	}
	if seen[`{"event":"e2"}`] != 1 || seen[`{"event":"e3"}`] != 1 {
		t.Fatalf("expected e2 and e3 exactly once each, got %v", seen)
	}
	if seen[`{"event":"e1"}`] != 0 {
		t.Fatalf("e1 redelivered after restart: %v", seen)
	}

	// Make sure no late redelivery of e1 sneaks in either.
	select {
	case data := <-got:
		t.Fatalf("unexpected extra delivery after restart: %q", data)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestConsumeAllDeliveryDurable_NakRedelivers asserts a handler error causes
// JetStream to redeliver the message (Nak triggers instant redelivery).
func TestConsumeAllDeliveryDurable_NakRedelivers(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	if err := bus.PublishDelivery("env1", []byte(`{"event":"retry-me"}`)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	var attempts int32
	got := make(chan []byte, 10)
	stop, err := bus.ConsumeAllDeliveryDurable(ctx, "wh-nak", func(data []byte) error {
		n := atomic.AddInt32(&attempts, 1)
		got <- data
		if n == 1 {
			return fmt.Errorf("simulated failure on first delivery")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeAllDeliveryDurable: %v", err)
	}
	defer func() { _ = stop() }()

	deliveries := 0
	deadline := time.After(5 * time.Second)
	for deliveries < 2 {
		select {
		case <-got:
			deliveries++
		case <-deadline:
			t.Fatalf("timed out after %d deliveries, want >= 2", deliveries)
		}
	}
	if atomic.LoadInt32(&attempts) < 2 {
		t.Fatalf("attempts = %d, want >= 2", attempts)
	}
}
