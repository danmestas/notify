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
// Relationship to ThrottleAllow: both are casSpend over a fixed-window key
// (the window epoch is baked into the key because NATS KV TTL is bucket-wide,
// not per-key, so a distinct window must be a distinct key that starts at
// zero). BudgetAllow is cost-weighted: a batch POST spends its whole
// byte/event weight in one call. ThrottleAllow spends 1.
//
// Failure mode is fail-CLOSED for every unresolved outcome: a KV transport
// error and CAS-retry exhaustion both return (false, err) so the ingest caller
// rejects the request (503) rather than admit unmetered bytes when the broker
// is unhealthy or contended. ThrottleAllow's caller makes the opposite call
// for the same (false, err); the counter itself never guesses.

// maxCASRetries bounds the read-modify-write CAS loop. Under realistic
// per-key concurrency a spend resolves in one or two attempts; exhausting
// this many consecutive lost races means the key is pathologically contended
// and the limiter reports that rather than spin or guess.
const maxCASRetries = 16

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
	return casSpend(context.Background(), b.budget, "budget", budgetKey(key, window), cost, limit)
}

// casSpend adds cost to the counter at bucketKey if the result stays within
// limit, under a revision compare-and-swap so concurrent spenders serialise
// on the key. name labels errors ("throttle", "budget").
//
// Returns:
//   - (true, nil):  the spend fit and was durably recorded.
//   - (false, nil): the spend would exceed the limit; nothing was written.
//   - (false, err): the outcome could not be confirmed (KV transport error or
//     CAS-retry exhaustion). Never (true, nil) for a spend that was not
//     confirmed; the caller decides whether an unresolved spend is admitted.
func casSpend(ctx context.Context, kv jetstream.KeyValue, name, bucketKey string, cost, limit int64) (bool, error) {
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		entry, getErr := kv.Get(ctx, bucketKey)
		switch {
		case getErr == nil:
			count, perr := strconv.ParseInt(string(entry.Value()), 10, 64)
			if perr != nil {
				return false, fmt.Errorf("eventbus: %s parse count for %q: %w", name, bucketKey, perr)
			}
			if count+cost > limit {
				return false, nil
			}
			next := []byte(strconv.FormatInt(count+cost, 10))
			if _, uerr := kv.Update(ctx, bucketKey, next, entry.Revision()); uerr != nil {
				if isBudgetCASConflict(uerr) {
					continue // lost the race; re-read and retry
				}
				return false, fmt.Errorf("eventbus: %s update %q: %w", name, bucketKey, uerr)
			}
			return true, nil

		case errors.Is(getErr, jetstream.ErrKeyNotFound):
			// First spend in this window: no key yet. Deny outright if a single
			// spend already exceeds the limit (there is nothing to create).
			if cost > limit {
				return false, nil
			}
			next := []byte(strconv.FormatInt(cost, 10))
			if _, cerr := kv.Create(ctx, bucketKey, next); cerr != nil {
				if isBudgetCASConflict(cerr) {
					continue // another spender created it first; retry as Update
				}
				return false, fmt.Errorf("eventbus: %s create %q: %w", name, bucketKey, cerr)
			}
			return true, nil

		default:
			return false, fmt.Errorf("eventbus: %s get %q: %w", name, bucketKey, getErr)
		}
	}

	// Every CAS attempt lost its race: the spend could not be confirmed. A
	// counter that cannot confirm a spend must say so, exactly like a KV
	// transport error.
	return false, fmt.Errorf("eventbus: %s: CAS retries exhausted for %q", name, bucketKey)
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
