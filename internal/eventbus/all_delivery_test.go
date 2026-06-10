package eventbus

import (
	"context"
	"testing"
	"time"
)

func TestBus_SubscribeAllDelivery_DeliversAllEnvs(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 2)
	unsub, err := bus.SubscribeAllDelivery(func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeAllDelivery: %v", err)
	}
	defer func() { _ = unsub() }()

	if err := bus.PublishDelivery("env1", []byte(`{"event":"one"}`)); err != nil {
		t.Fatalf("PublishDelivery(env1): %v", err)
	}
	if err := bus.PublishDelivery("env2", []byte(`{"event":"two"}`)); err != nil {
		t.Fatalf("PublishDelivery(env2): %v", err)
	}

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case data := <-got:
			seen[string(data)] = true
		case <-deadline:
			t.Fatalf("timed out; only received %v", seen)
		}
	}
	if !seen[`{"event":"one"}`] || !seen[`{"event":"two"}`] {
		t.Fatalf("expected both env events, got %v", seen)
	}
}
