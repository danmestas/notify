package eventbus

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newBudgetBus spins up a sidecar and returns a connected Bus for budget tests.
func newBudgetBus(t *testing.T) *Bus {
	t.Helper()
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

// TestBudgetAllow_UnderAndOver — cost-weighted spends up to the limit are
// allowed; the spend that crosses the limit is denied without recording, and
// subsequent spends within the same window stay denied.
func TestBudgetAllow_UnderAndOver(t *testing.T) {
	bus := newBudgetBus(t)

	const (
		key    = "src-A"
		limit  = int64(100)
		window = time.Hour
	)

	// Spend 40 + 40 = 80 (<= 100): both allowed.
	for i, cost := range []int64{40, 40} {
		allowed, err := bus.BudgetAllow(key, cost, limit, window)
		if err != nil {
			t.Fatalf("BudgetAllow spend #%d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("BudgetAllow spend #%d (cost=%d): got denied, want allowed", i, cost)
		}
	}

	// Spend 40 more would be 120 (> 100): denied, and NOT recorded.
	if allowed, err := bus.BudgetAllow(key, 40, limit, window); err != nil || allowed {
		t.Fatalf("BudgetAllow over-limit: allowed=%v err=%v, want allowed=false err=nil", allowed, err)
	}

	// A spend that still fits (80 + 20 = 100) must be allowed — proving the
	// denied over-limit spend above did not consume budget.
	if allowed, err := bus.BudgetAllow(key, 20, limit, window); err != nil || !allowed {
		t.Fatalf("BudgetAllow exact-fit after denial: allowed=%v err=%v, want allowed=true", allowed, err)
	}

	// Now at 100/100: any further positive spend is denied.
	if allowed, err := bus.BudgetAllow(key, 1, limit, window); err != nil || allowed {
		t.Fatalf("BudgetAllow at cap: allowed=%v err=%v, want allowed=false", allowed, err)
	}
}

// TestBudgetAllow_IndependentKeys — exhausting key A must not affect key B
// (per-source isolation).
func TestBudgetAllow_IndependentKeys(t *testing.T) {
	bus := newBudgetBus(t)
	const window = time.Hour

	if allowed, err := bus.BudgetAllow("A", 10, 10, window); err != nil || !allowed {
		t.Fatalf("BudgetAllow(A) fill: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := bus.BudgetAllow("A", 1, 10, window); err != nil || allowed {
		t.Fatalf("BudgetAllow(A) over: allowed=%v err=%v, want denied", allowed, err)
	}
	// B is a different key and must be unaffected.
	if allowed, err := bus.BudgetAllow("B", 10, 10, window); err != nil || !allowed {
		t.Fatalf("BudgetAllow(B): allowed=%v err=%v, want allowed", allowed, err)
	}
}

// TestBudgetAllow_ZeroAndNegativeCost — non-positive cost is a caller error,
// returned as (false, err), never silently allowed.
func TestBudgetAllow_ZeroAndNegativeCost(t *testing.T) {
	bus := newBudgetBus(t)
	const window = time.Hour

	for _, cost := range []int64{0, -1, -1000} {
		allowed, err := bus.BudgetAllow("k", cost, 100, window)
		if err == nil {
			t.Fatalf("BudgetAllow(cost=%d): got err=nil, want error", cost)
		}
		if allowed {
			t.Fatalf("BudgetAllow(cost=%d): got allowed=true, want false", cost)
		}
	}
}

// TestBudgetAllow_WindowExpiry — with a short window, a fresh window starts at
// zero. The window epoch is baked into the KV key, so a later call lands on a
// distinct key that naturally counts from zero.
func TestBudgetAllow_WindowExpiry(t *testing.T) {
	bus := newBudgetBus(t)
	const (
		key    = "expiry"
		limit  = int64(1)
		window = time.Second
	)

	// Align to the very start of a fresh window so the two same-window spends
	// below can't straddle an epoch boundary (which would put the 2nd on a new
	// epoch key and wrongly allow it — a CI flake).
	intoWindow := time.Duration(time.Now().UnixNano() % window.Nanoseconds())
	time.Sleep(window - intoWindow + 20*time.Millisecond)

	if allowed, err := bus.BudgetAllow(key, 1, limit, window); err != nil || !allowed {
		t.Fatalf("BudgetAllow first: allowed=%v err=%v, want allowed", allowed, err)
	}
	if allowed, err := bus.BudgetAllow(key, 1, limit, window); err != nil || allowed {
		t.Fatalf("BudgetAllow second (same window): allowed=%v err=%v, want denied", allowed, err)
	}

	// Sleep just past the window so the next call lands in a fresh epoch key.
	time.Sleep(window + 200*time.Millisecond)

	if allowed, err := bus.BudgetAllow(key, 1, limit, window); err != nil || !allowed {
		t.Fatalf("BudgetAllow after window: allowed=%v err=%v, want allowed", allowed, err)
	}
}

// TestConnect_EnsuresBudgetBucket — the ingest-budget KV bucket is created in
// Connect and a second Connect is idempotent.
func TestConnect_EnsuresBudgetBucket(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus1, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	defer bus1.Close()

	js := inspectJS(t, natsURL)
	if _, err := js.KeyValue(ctx, budgetBucket); err != nil {
		t.Fatalf("KeyValue(%q): %v", budgetBucket, err)
	}

	bus2, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("second Connect (idempotent): %v", err)
	}
	defer bus2.Close()
}

// TestBudgetAllow_ConcurrentCAS — many goroutines spend concurrently on one key
// whose budget is smaller than total demand. Under -race this proves the two
// correctness properties a byte budget requires, which hold under ANY contention:
//
//   - never over-admit: the admitted total never exceeds the limit; and
//   - no lost / double updates: the stored counter equals exactly the admitted
//     total (a best-effort Get/Put would lose concurrent increments, leaving
//     stored < admitted — the undercount this guard exists to prevent).
//
// Synthetic hyper-contention can legitimately exhaust the bounded CAS loop; such
// a spender fails CLOSED (allowed=false, err!=nil) and is neither admitted nor
// written, so both invariants are preserved. After the wave we deterministically
// fill the remaining budget sequentially to prove the counter sits at exactly
// `admitted` and denies past `limit`.
func TestBudgetAllow_ConcurrentCAS(t *testing.T) {
	bus := newBudgetBus(t)

	const (
		key        = "concurrent"
		limit      = int64(50)
		goroutines = 200
		window     = time.Hour
	)

	var admitted, errored int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			allowed, err := bus.BudgetAllow(key, 1, limit, window)
			switch {
			case err != nil:
				// Fail-closed under contention: must never also report allowed.
				if allowed {
					t.Errorf("BudgetAllow returned allowed=true with err=%v (must fail closed)", err)
				}
				atomic.AddInt64(&errored, 1)
			case allowed:
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	wg.Wait()

	adm := atomic.LoadInt64(&admitted)
	if adm > limit {
		t.Fatalf("over-admit: admitted %d spends, limit is %d", adm, limit)
	}
	if adm == 0 {
		t.Fatalf("no spends admitted (limiter failed closed for everything)")
	}
	t.Logf("concurrent wave: admitted=%d errored(fail-closed)=%d", adm, atomic.LoadInt64(&errored))

	// The stored counter must equal exactly the admitted total — no lost or
	// double-counted updates.
	stored := readBudgetCounter(t, bus, key, window)
	if stored != adm {
		t.Fatalf("stored counter = %d, want %d (CAS lost or double-counted updates)", stored, adm)
	}

	// Fill the remaining budget sequentially: every one must be allowed, proving
	// the counter really is at `adm` and the CAS admits cleanly up to the cap.
	for spent := adm; spent < limit; spent++ {
		if allowed, err := bus.BudgetAllow(key, 1, limit, window); err != nil || !allowed {
			t.Fatalf("sequential fill at %d/%d: allowed=%v err=%v, want allowed", spent, limit, allowed, err)
		}
	}

	// At the cap now: the next spend is denied without error, and the counter is
	// exactly the limit.
	if allowed, err := bus.BudgetAllow(key, 1, limit, window); err != nil || allowed {
		t.Fatalf("spend past cap: allowed=%v err=%v, want allowed=false err=nil", allowed, err)
	}
	if stored := readBudgetCounter(t, bus, key, window); stored != limit {
		t.Fatalf("final stored counter = %d, want %d", stored, limit)
	}
}

// readBudgetCounter reads and parses the raw stored budget count for the current
// window of key.
func readBudgetCounter(t *testing.T, bus *Bus, key string, window time.Duration) int64 {
	t.Helper()
	entry, err := bus.budget.Get(context.Background(), budgetKey(key, window))
	if err != nil {
		t.Fatalf("read budget counter: %v", err)
	}
	n, err := strconv.ParseInt(string(entry.Value()), 10, 64)
	if err != nil {
		t.Fatalf("parse budget counter %q: %v", entry.Value(), err)
	}
	return n
}

// TestBudgetAllow_FailClosedOnKVError — when the KV layer is unavailable, a
// limiter that cannot confirm the spend must fail CLOSED: (false, err), so the
// retention guard is never disarmed by an unhealthy broker.
func TestBudgetAllow_FailClosedOnKVError(t *testing.T) {
	bus := newBudgetBus(t)

	// Drop the connection out from under the limiter to inject a KV transport
	// error on the next Get.
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	allowed, err := bus.BudgetAllow("k", 1, 100, time.Hour)
	if err == nil {
		t.Fatalf("BudgetAllow after broker down: got err=nil, want error (fail-closed)")
	}
	if allowed {
		t.Fatalf("BudgetAllow after broker down: got allowed=true, want false (fail-closed)")
	}
}
