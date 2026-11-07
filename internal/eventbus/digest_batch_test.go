package eventbus

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// TestDigestAppend_ConcurrentLosesNothing pins the atomic append: 50 racing
// appenders on one key must leave 50 events (a plain Get/Put would drop some).
func TestDigestAppend_ConcurrentLosesNothing(t *testing.T) {
	bus := newDigestBus(t)
	const key = "env:sub:wf:concurrent"
	const n = 50

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- bus.DigestAppend(key, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("DigestAppend: %v", err)
		}
	}

	events, err := bus.DigestBatch(key, "run-1")
	if err != nil {
		t.Fatalf("DigestBatch: %v", err)
	}
	if len(events) != n {
		t.Fatalf("got %d events, want %d", len(events), n)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[string(e)] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct events, want %d", len(seen), n)
	}
}

// TestDigestBatch_SameBatchForEveryStepOfARun proves the window closes once per
// run: every later call (another channel step, or a retry) gets the full batch.
func TestDigestBatch_SameBatchForEveryStepOfARun(t *testing.T) {
	bus := newDigestBus(t)
	const key = "env:sub:wf:batch"
	for i := range 5 {
		if err := bus.DigestAppend(key, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatalf("DigestAppend: %v", err)
		}
	}
	for step := range 3 {
		events, err := bus.DigestBatch(key, "run-1")
		if err != nil {
			t.Fatalf("DigestBatch step %d: %v", step, err)
		}
		if len(events) != 5 {
			t.Fatalf("step %d saw %d events, want 5", step, len(events))
		}
	}
}

// TestDigestBatch_LateEventOpensNextWindow proves an event arriving after the
// window closed is not lost, is not leaked into the closed run's batch, and
// starts a fresh window (claim cleared).
func TestDigestBatch_LateEventOpensNextWindow(t *testing.T) {
	bus := newDigestBus(t)
	const key = "env:sub:wf:late"

	if owner, err := bus.DigestClaim(key); err != nil || !owner {
		t.Fatalf("first claim: owner=%v err=%v", owner, err)
	}
	if err := bus.DigestAppend(key, json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.DigestBatch(key, "run-1"); err != nil {
		t.Fatal(err)
	}

	owner, err := bus.DigestClaim(key)
	if err != nil || !owner {
		t.Fatalf("post-close claim: owner=%v err=%v (the next window must open)", owner, err)
	}
	if err := bus.DigestAppend(key, json.RawMessage(`{"n":2}`)); err != nil {
		t.Fatal(err)
	}

	closed, err := bus.DigestBatch(key, "run-1")
	if err != nil || len(closed) != 1 {
		t.Fatalf("closed run batch = %d events, err=%v; want its original 1", len(closed), err)
	}
	next, err := bus.DigestBatch(key, "run-2")
	if err != nil || len(next) != 1 || string(next[0]) != `{"n":2}` {
		t.Fatalf("next window batch = %s, err=%v; want [{\"n\":2}]", next, err)
	}
}
