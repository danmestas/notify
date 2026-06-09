package eventbus

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func newDigestBus(t *testing.T) *Bus {
	t.Helper()
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

func TestBus_DigestAppendAndDrain(t *testing.T) {
	bus := newDigestBus(t)

	const key = "sub-1:wf-1:order-42"
	e1 := json.RawMessage(`{"n":1}`)
	e2 := json.RawMessage(`{"n":2}`)

	if err := bus.DigestAppend(key, e1); err != nil {
		t.Fatalf("DigestAppend e1: %v", err)
	}
	if err := bus.DigestAppend(key, e2); err != nil {
		t.Fatalf("DigestAppend e2: %v", err)
	}

	events, err := bus.DigestDrain(key)
	if err != nil {
		t.Fatalf("DigestDrain: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("DigestDrain: got %d events, want 2", len(events))
	}
	if string(events[0]) != string(e1) || string(events[1]) != string(e2) {
		t.Fatalf("DigestDrain: got [%s, %s], want [%s, %s]", events[0], events[1], e1, e2)
	}

	again, err := bus.DigestDrain(key)
	if err != nil {
		t.Fatalf("DigestDrain second: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("DigestDrain second: got %d events, want 0 (cleared)", len(again))
	}
}

func TestBus_DigestClaim_OnlyOneOwner(t *testing.T) {
	bus := newDigestBus(t)

	const key = "sub-2:wf-2:digest"

	owner, err := bus.DigestClaim(key)
	if err != nil {
		t.Fatalf("DigestClaim first: %v", err)
	}
	if !owner {
		t.Fatalf("DigestClaim first: got isOwner=false, want true")
	}

	for i := 2; i <= 3; i++ {
		owner, err := bus.DigestClaim(key)
		if err != nil {
			t.Fatalf("DigestClaim #%d: %v", i, err)
		}
		if owner {
			t.Fatalf("DigestClaim #%d: got isOwner=true, want false", i)
		}
	}

	if _, err := bus.DigestDrain(key); err != nil {
		t.Fatalf("DigestDrain: %v", err)
	}

	owner, err = bus.DigestClaim(key)
	if err != nil {
		t.Fatalf("DigestClaim after drain: %v", err)
	}
	if !owner {
		t.Fatalf("DigestClaim after drain: got isOwner=false, want true (claim cleared)")
	}
}

func TestBus_DigestSeparateKeys(t *testing.T) {
	bus := newDigestBus(t)

	ea := json.RawMessage(`{"k":"a"}`)
	eb := json.RawMessage(`{"k":"b"}`)

	if err := bus.DigestAppend("a", ea); err != nil {
		t.Fatalf("DigestAppend(a): %v", err)
	}
	if err := bus.DigestAppend("b", eb); err != nil {
		t.Fatalf("DigestAppend(b): %v", err)
	}

	ownerA, err := bus.DigestClaim("a")
	if err != nil || !ownerA {
		t.Fatalf("DigestClaim(a): owner=%v err=%v, want true", ownerA, err)
	}
	// Claiming "a" must not affect "b".
	ownerB, err := bus.DigestClaim("b")
	if err != nil || !ownerB {
		t.Fatalf("DigestClaim(b): owner=%v err=%v, want true", ownerB, err)
	}

	eventsA, err := bus.DigestDrain("a")
	if err != nil {
		t.Fatalf("DigestDrain(a): %v", err)
	}
	if len(eventsA) != 1 || string(eventsA[0]) != string(ea) {
		t.Fatalf("DigestDrain(a): got %v, want [%s]", eventsA, ea)
	}

	// Draining "a" must leave "b" intact.
	eventsB, err := bus.DigestDrain("b")
	if err != nil {
		t.Fatalf("DigestDrain(b): %v", err)
	}
	if len(eventsB) != 1 || string(eventsB[0]) != string(eb) {
		t.Fatalf("DigestDrain(b): got %v, want [%s]", eventsB, eb)
	}
}
