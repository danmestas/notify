package eventbus

import (
	"context"
	"testing"
	"time"
)

func TestBus_InboxPubSub_DeliversToSubscriber(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 1)
	unsub, err := bus.SubscribeInbox("env1", "sub1", func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeInbox: %v", err)
	}
	defer unsub()

	want := []byte(`{"id":"m1"}`)
	if err := bus.PublishInbox("env1", "sub1", want); err != nil {
		t.Fatalf("PublishInbox: %v", err)
	}

	select {
	case data := <-got:
		if string(data) != string(want) {
			t.Fatalf("payload = %q, want %q", data, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbox event")
	}
}

func TestBus_InboxPubSub_ScopedBySubject(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 1)
	unsub, err := bus.SubscribeInbox("env1", "sub1", func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeInbox: %v", err)
	}
	defer unsub()

	// Event for a different subscriber must NOT reach sub1's handler.
	if err := bus.PublishInbox("env1", "sub2", []byte(`{"id":"other"}`)); err != nil {
		t.Fatalf("PublishInbox(sub2): %v", err)
	}
	select {
	case data := <-got:
		t.Fatalf("sub1 handler received cross-subject event: %q", data)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing delivered
	}

	// Event for sub1 DOES reach the handler.
	want := []byte(`{"id":"mine"}`)
	if err := bus.PublishInbox("env1", "sub1", want); err != nil {
		t.Fatalf("PublishInbox(sub1): %v", err)
	}
	select {
	case data := <-got:
		if string(data) != string(want) {
			t.Fatalf("payload = %q, want %q", data, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sub1 inbox event")
	}
}

func TestBus_InboxUnsubscribe_Stops(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	got := make(chan []byte, 1)
	unsub, err := bus.SubscribeInbox("env1", "sub1", func(data []byte) {
		got <- data
	})
	if err != nil {
		t.Fatalf("SubscribeInbox: %v", err)
	}
	if err := unsub(); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	if err := bus.PublishInbox("env1", "sub1", []byte(`{"id":"after"}`)); err != nil {
		t.Fatalf("PublishInbox: %v", err)
	}
	select {
	case data := <-got:
		t.Fatalf("handler received event after unsubscribe: %q", data)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing delivered
	}
}
