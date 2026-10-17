package main

import (
	"context"
	"log/slog"
	"time"

	agentstore "github.com/danmestas/notify/internal/agents/store"

	"github.com/danmestas/notify/internal/store"
)

const (
	// stepOutputRetention bounds how long captured http_request responses live
	// in step_outputs.
	//
	// Deliberately the same 7 days as the delivery stream's retention rather
	// than a second, independently-chosen number: one retention window is one
	// thing to reason about, and two tend to drift until nobody can say what
	// the system's actual guarantee is. Seven days covers debugging a recent
	// run; anything older is archaeology, and notification history -- retained
	// separately -- already serves audit.
	//
	// The rows hold third-party HTTP response bodies, data notify did not
	// author and cannot vouch for, so holding less of it for less time is the
	// better default.
	stepOutputRetention = 7 * 24 * time.Hour

	// stepOutputPurgeInterval is how often the purge runs. Retention is a
	// ceiling, not a deadline, so an interval well under the window keeps the
	// table bounded without the purge itself being load.
	stepOutputPurgeInterval = 6 * time.Hour

	// delayDeadlinePurgeInterval is how often the delay_deadlines purge runs.
	// store.MaxDelayDeadlineAge is measured in months, so this can be far
	// coarser than stepOutputPurgeInterval without letting the table grow
	// unboundedly between runs.
	delayDeadlinePurgeInterval = 24 * time.Hour
)

// startStepOutputPurge runs the retention purge once immediately, then on an
// interval until ctx is cancelled.
//
// The immediate pass matters: without it a process that restarts more often
// than the interval would never purge at all, which is exactly the deployment
// pattern this codebase has.
//
// Failures are logged and the loop continues. A purge that cannot run is a
// table that grows, not a request that fails, so it must never take the process
// down with it.
func startStepOutputPurge(ctx context.Context, st *store.Store) {
	purge := func() {
		cutoff := time.Now().Add(-stepOutputRetention)
		n, err := st.StepOutputs().PurgeOlderThan(ctx, cutoff)
		switch {
		case err != nil:
			slog.Default().Error("retention: step_outputs purge failed",
				"err", err, "cutoff", cutoff)
		case n > 0:
			slog.Default().Info("retention: purged step_outputs",
				"rows", n, "olderThan", stepOutputRetention.String())
		}
	}

	go func() {
		purge()
		t := time.NewTicker(stepOutputPurgeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
			}
		}
	}()
}

// startDelayDeadlinePurge runs the delay_deadlines retention purge once
// immediately, then on an interval until ctx is cancelled.
//
// Rows here used to be swept only as a side effect of DelayDeadlines().Save,
// which meant a deployment that stopped creating new relative dynamic delays
// stopped sweeping too -- exactly the unbounded growth retention exists to
// prevent. Moving to the same boot-plus-ticker shape as startStepOutputPurge
// closes that gap: the purge now runs regardless of traffic.
//
// The immediate pass matters for the same reason as startStepOutputPurge's:
// a process that restarts more often than the interval would otherwise never
// purge at all.
//
// Failures are logged and the loop continues. A purge that cannot run is a
// table that grows, not a request that fails, so it must never take the
// process down with it.
func startDelayDeadlinePurge(ctx context.Context, st *store.Store) {
	purge := func() {
		cutoff := time.Now().Add(-store.MaxDelayDeadlineAge)
		n, err := st.DelayDeadlines().PurgeOlderThan(ctx, cutoff)
		switch {
		case err != nil:
			slog.Default().Error("retention: delay_deadlines purge failed",
				"err", err, "cutoff", cutoff)
		case n > 0:
			slog.Default().Info("retention: purged delay_deadlines",
				"rows", n, "olderThan", store.MaxDelayDeadlineAge.String())
		}
	}

	go func() {
		purge()
		t := time.NewTicker(delayDeadlinePurgeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
			}
		}
	}()
}

// draftPurgeInterval is how often expired agent drafts are deleted. Hourly is
// ample against a 30-day retention.
const draftPurgeInterval = time.Hour

// startDraftPurge deletes agent drafts older than agentstore.DraftRetention,
// once at boot and then on an interval, in the same shape as the purges above.
// A reader whose cursor predates the window is told so by the endpoint (gap),
// so nothing expires unannounced.
func startDraftPurge(ctx context.Context, ags *agentstore.Store) {
	purge := func() {
		cutoff := time.Now().Add(-agentstore.DraftRetention)
		n, err := ags.Drafts().PurgeOlderThan(ctx, cutoff)
		switch {
		case err != nil:
			slog.Default().Error("retention: agent_drafts purge failed", "err", err, "cutoff", cutoff)
		case n > 0:
			slog.Default().Info("retention: purged agent_drafts", "rows", n, "olderThan", agentstore.DraftRetention.String())
		}
	}

	go func() {
		purge()
		t := time.NewTicker(draftPurgeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
			}
		}
	}()
}
