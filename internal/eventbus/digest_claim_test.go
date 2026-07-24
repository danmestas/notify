package eventbus

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// A digest window is claimed once (the first event's trigger becomes owner and
// starts the windowed run) and released only when the window drains. Nothing
// refreshes the claim in between, so a crash between claim and drain used to
// leave the claim behind forever: every later digest for that subscriber/key
// saw a live claim, backed off, and the subscriber silently stopped receiving
// digests for that key with no self-healing path.
//
// The fix moves claims into their own KV bucket with a bucket-wide TTL, so an
// orphaned claim expires on its own WITHOUT putting any lifetime on the
// accumulating event lists (which stay in the digest bucket). These tests pin
// both halves of that: claims expire, event lists do not.

// newDigestBusTTL builds a bus whose claim bucket expires claims after ttl, so
// orphan-expiry is testable in seconds rather than the production window.
func newDigestBusTTL(t *testing.T, ttl time.Duration) *Bus {
	t.Helper()
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL, WithDigestClaimTTL(ttl))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

// TestDigestClaim_OwnerExactlyOnce pins the atomic single-owner guarantee that
// keeps a window from being started twice: many callers, exactly one owner.
func TestDigestClaim_OwnerExactlyOnce(t *testing.T) {
	bus := newDigestBus(t)
	const key = "env:sub:wf:window"

	const callers = 8
	var wg sync.WaitGroup
	owners := make([]bool, callers)
	wg.Add(callers)
	for i := range callers {
		go func(i int) {
			defer wg.Done()
			owner, err := bus.DigestClaim(key)
			if err != nil {
				t.Errorf("DigestClaim: %v", err)
				return
			}
			owners[i] = owner
		}(i)
	}
	wg.Wait()

	count := 0
	for _, o := range owners {
		if o {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent claims produced %d owners, want exactly 1", count)
	}
}

// TestDigestClaim_OrphanExpires is the regression test for the permanent lock:
// a claim that is never drained must stop blocking new claims once its lifetime
// elapses, so digests resume on their own.
func TestDigestClaim_OrphanExpires(t *testing.T) {
	const ttl = 2 * time.Second
	bus := newDigestBusTTL(t, ttl)
	const key = "env:sub:wf:orphaned"

	owner, err := bus.DigestClaim(key)
	if err != nil {
		t.Fatalf("first DigestClaim: %v", err)
	}
	if !owner {
		t.Fatal("first claim was not owner")
	}

	// Simulate the crash: the window is never drained, so the claim is never
	// released. A second claim while it is live must back off.
	again, err := bus.DigestClaim(key)
	if err != nil {
		t.Fatalf("second DigestClaim: %v", err)
	}
	if again {
		t.Fatal("a live claim did not block the second caller")
	}

	// After the lifetime elapses, the orphaned claim must be gone and a new
	// claim must succeed -- the subscriber's digests resume without operator
	// intervention.
	waitUntilClaimFree(t, bus, key, ttl+3*time.Second)
}

// waitUntilClaimFree polls DigestClaim until it succeeds or the deadline passes.
// Polling (rather than a single sleep of exactly ttl) tolerates the sidecar's
// expiry granularity without making the test flaky.
func waitUntilClaimFree(t *testing.T, bus *Bus, key string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		owner, err := bus.DigestClaim(key)
		if err != nil {
			t.Fatalf("DigestClaim while waiting for expiry: %v", err)
		}
		if owner {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("orphaned claim on %q never expired within %s: the lock is still permanent", key, within)
}

// TestDigestClaim_EventListsSurviveClaimExpiry is the other half of the fix:
// making claims expire must NOT put a lifetime on accumulating events. Losing
// buffered digest events to expire a claim would be a strictly worse bug than
// the one being fixed.
func TestDigestClaim_EventListsSurviveClaimExpiry(t *testing.T) {
	const ttl = 2 * time.Second
	bus := newDigestBusTTL(t, ttl)
	const key = "env:sub:wf:accumulating"

	if _, err := bus.DigestClaim(key); err != nil {
		t.Fatalf("DigestClaim: %v", err)
	}
	e1 := json.RawMessage(`{"n":1}`)
	e2 := json.RawMessage(`{"n":2}`)
	if err := bus.DigestAppend(key, e1); err != nil {
		t.Fatalf("DigestAppend e1: %v", err)
	}
	if err := bus.DigestAppend(key, e2); err != nil {
		t.Fatalf("DigestAppend e2: %v", err)
	}

	// Wait past the claim lifetime. If the events shared the claim's TTL they
	// would be gone now.
	time.Sleep(ttl + 2*time.Second)

	events, err := bus.DigestDrain(key)
	if err != nil {
		t.Fatalf("DigestDrain: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("accumulated events after claim expiry = %d, want 2: event lists inherited the claim TTL", len(events))
	}
}

// TestDigestClaim_NormalCycleUnchanged keeps the ordinary path working: claim,
// drain (which releases the claim), then a fresh claim for the next window.
func TestDigestClaim_NormalCycleUnchanged(t *testing.T) {
	bus := newDigestBus(t)
	const key = "env:sub:wf:cycle"

	owner, err := bus.DigestClaim(key)
	if err != nil || !owner {
		t.Fatalf("first claim: owner=%v err=%v", owner, err)
	}
	if _, err := bus.DigestDrain(key); err != nil {
		t.Fatalf("DigestDrain: %v", err)
	}
	// Drain released the claim, so the next window claims cleanly.
	owner, err = bus.DigestClaim(key)
	if err != nil || !owner {
		t.Fatalf("post-drain claim: owner=%v err=%v", owner, err)
	}
}
