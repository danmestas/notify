package eventbus

import (
	"context"
	"testing"
	"time"
)

func TestBus_DeliveryPubSub_DeliversToEnv(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 1)
	unsub, err := bus.SubscribeDelivery("env1", func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeDelivery: %v", err)
	}
	defer unsub()

	want := []byte(`{"event":"sent"}`)
	if err := bus.PublishDelivery("env1", want); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	select {
	case data := <-got:
		if string(data) != string(want) {
			t.Fatalf("payload = %q, want %q", data, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery event")
	}
}

func TestBus_DeliveryPubSub_ScopedByEnv(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 1)
	unsub, err := bus.SubscribeDelivery("env1", func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeDelivery: %v", err)
	}
	defer unsub()

	// Event for a different environment must NOT reach env1's handler.
	if err := bus.PublishDelivery("env2", []byte(`{"event":"other"}`)); err != nil {
		t.Fatalf("PublishDelivery(env2): %v", err)
	}
	select {
	case data := <-got:
		t.Fatalf("handler received event for env2: %q", data)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing delivered
	}

	// Event for env1 DOES reach the handler.
	want := []byte(`{"event":"mine"}`)
	if err := bus.PublishDelivery("env1", want); err != nil {
		t.Fatalf("PublishDelivery(env1): %v", err)
	}
	select {
	case data := <-got:
		if string(data) != string(want) {
			t.Fatalf("payload = %q, want %q", data, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for env1 delivery event")
	}
}

func TestPublishDelivery_EmptyEnvRejected(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	if err := bus.PublishDelivery("", []byte(`{"event":"x"}`)); err == nil {
		t.Fatal("PublishDelivery(\"\", ...) = nil error, want error")
	}
}

func TestBus_DeliveryUnsubscribe_Stops(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 1)
	unsub, err := bus.SubscribeDelivery("env1", func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeDelivery: %v", err)
	}
	if err := unsub(); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	if err := bus.PublishDelivery("env1", []byte(`{"event":"after"}`)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	select {
	case data := <-got:
		t.Fatalf("handler received event after unsubscribe: %q", data)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing delivered
	}
}
