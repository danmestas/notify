package main

import (
	"context"
	"log/slog"
	"time"

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
