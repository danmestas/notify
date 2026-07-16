package eventbus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// budget.go implements BudgetAllow, a cost-weighted fixed-window limiter over
// NATS KV. It is the retention guard for inbound webhook-push ingest: the
// NOTIFY_FEED stream is a single 1 GiB DiscardOld buffer shared by every
// tenant, so an unbounded firehose source would evict every other tenant's
// replay window. BudgetAllow caps how much a source may write per window.
//
// Retention arithmetic: with feedingest's default 2 MiB/hour byte budget per
// source, a maxed-out source writes at most 2 MiB * 24 * 7 = 336 MiB across the
// stream's 7-day MaxAge window — comfortably under the 1 GiB stream cap, so one
// source can no longer evict the whole stream. This fixed-window limiter admits
// a small boundary burst (up to one full window's budget can land just before
// and just after a window boundary), but that at most doubles a single window's
// spend and is negligible against the 1 GiB headroom. The cap remains
// stream-wide across ALL sources; a handful of maxed sources could still
// saturate it. That residual is accepted for the MVP; per-env streams /
// per-tenant accounting are the real fix (tracked, deferred).
//
// Relationship to ThrottleAllow: same fixed-window-over-KV shape and the same
// window-epoch-in-key trick (NATS KV TTL is bucket-wide, not per-key, so a
// distinct window must be a distinct key that naturally starts at zero). Two
// differences, both load-bearing for a byte budget:
//
//  1. Cost-weighted: a spend adds `cost` units, not 1 — a batch POST spends its
//     whole byte/event weight in one call.
//  2. Revision-CAS instead of best-effort Get/Put: ThrottleAllow tolerates a
//     tiny undercount under concurrency because over-admitting a rate limit is
//     minor. A byte budget cannot — an undercount is exactly the eviction this
//     guard exists to prevent — so BudgetAllow reads the revision and Updates
//     under CAS, retrying on conflict and NEVER recording a spend it did not
//     confirm.
//
// Failure mode is fail-CLOSED for every unresolved outcome: a KV transport
// error and CAS-retry exhaustion both return (false, err) so the ingest caller
// rejects the request (503) rather than admit unmetered bytes when the broker
// is unhealthy or contended.

// maxBudgetCASRetries bounds the read-modify-write CAS loop. Under realistic
// per-source concurrency a spend resolves in one or two attempts; exhausting
// this many consecutive lost races means the key is pathologically contended
// and the limiter fails closed rather than spin or guess.
const maxBudgetCASRetries = 16

// budgetKey builds the KV key for a caller key within the current fixed window.
// The window epoch (Unix-nanos / window) is baked into the key so each window is
// a distinct key that starts at zero — the same idiom ThrottleAllow uses,
// because NATS KV TTL is bucket-wide (a stream MaxAge), not per-key.
func budgetKey(key string, window time.Duration) string {
	return sanitizeKey(key) + "." +
		strconv.FormatInt(time.Now().UnixNano()/window.Nanoseconds(), 10)
}

// BudgetAllow reports whether spending `cost` units under `key` stays within
// `limit` for the current fixed `window`, and records the spend when allowed.
// Units are caller-defined (feedingest uses bytes on a 1h window and event
// counts on a 1m window). It mirrors ThrottleAllow's fixed-window-over-KV shape
// but is cost-weighted and uses a revision-CAS loop so a byte budget is never
// under- or over-counted under concurrency.
//
// Returns:
//   - (true, nil):  the spend fit and was durably recorded.
//   - (false, nil): the spend would exceed the limit; nothing was written.
//   - (false, err): the outcome could not be confirmed (non-positive cost/window,
//     KV transport error, or CAS-retry exhaustion). FAIL-CLOSED — the caller
//     must reject (503). An unresolved CAS never returns (true, nil) or
//     (false, nil).
func (b *Bus) BudgetAllow(key string, cost, limit int64, window time.Duration) (allowed bool, err error) {
	if cost <= 0 {
		return false, fmt.Errorf("eventbus: BudgetAllow: cost must be positive, got %d", cost)
	}
	if window <= 0 {
		return false, fmt.Errorf("eventbus: BudgetAllow: window must be positive, got %s", window)
	}

	ctx := context.Background()
	bucketKey := budgetKey(key, window)

	for attempt := 0; attempt < maxBudgetCASRetries; attempt++ {
		entry, getErr := b.budget.Get(ctx, bucketKey)
		switch {
		case getErr == nil:
			count, perr := strconv.ParseInt(string(entry.Value()), 10, 64)
			if perr != nil {
				return false, fmt.Errorf("eventbus: budget parse count for %q: %w", bucketKey, perr)
			}
			if count+cost > limit {
				return false, nil
			}
			next := []byte(strconv.FormatInt(count+cost, 10))
			if _, uerr := b.budget.Update(ctx, bucketKey, next, entry.Revision()); uerr != nil {
				if isBudgetCASConflict(uerr) {
					continue // lost the race; re-read and retry
				}
				return false, fmt.Errorf("eventbus: budget update %q: %w", bucketKey, uerr)
			}
			return true, nil

		case errors.Is(getErr, jetstream.ErrKeyNotFound):
			// First spend in this window: no key yet. Deny outright if a single
			// spend already exceeds the limit (there is nothing to create).
			if cost > limit {
				return false, nil
			}
			next := []byte(strconv.FormatInt(cost, 10))
			if _, cerr := b.budget.Create(ctx, bucketKey, next); cerr != nil {
				if isBudgetCASConflict(cerr) {
					continue // another spender created it first; retry as Update
				}
				return false, fmt.Errorf("eventbus: budget create %q: %w", bucketKey, cerr)
			}
			return true, nil

		default:
			return false, fmt.Errorf("eventbus: budget get %q: %w", bucketKey, getErr)
		}
	}

	// Every CAS attempt lost its race: the spend could not be confirmed. A
	// limiter that cannot confirm a spend must fail closed, exactly like a KV
	// transport error.
	return false, fmt.Errorf("eventbus: BudgetAllow: CAS retries exhausted for %q", bucketKey)
}

// isBudgetCASConflict reports whether err is a lost optimistic-concurrency race
// on the budget KV — either a Create that found the key already present or an
// Update whose expected revision no longer matched. NATS KV surfaces both as a
// wrong-last-sequence API error (ErrKeyExists is itself that error code), so a
// single ErrorCode check covers the create-race and the update-race alike.
func isBudgetCASConflict(err error) bool {
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
	}
	return false
}
