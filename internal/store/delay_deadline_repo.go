package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// MaxDelayDeadlineAge bounds how long an abandoned delay_deadlines row
// survives before the retention purge (see cmd/notify's
// startDelayDeadlinePurge) reclaims it. It is a backstop for runs that crash
// or are cancelled before reaching normal completion (HandleDynamic calls
// Clear on every successful completion, so a healthy run's row never reaches
// this age) -- NOT a wait-duration limit: a currently-waiting relative delay
// never re-resolves once its deadline row exists, so nothing here can cut a
// live wait short.
//
// It must exceed the longest relative delay HandleDynamic can resolve.
// steptype.UnitDuration's largest unit is "months" (a 30-day approximation);
// the {amount,unit} shape places no upper bound on amount at the type level,
// but the realistic ceiling an author reaches for with that vocabulary is a
// year-long reminder (12 months). 400 days covers 12 months (360 days at the
// 30-day approximation) plus 40 days of grace, so an abandoned row is never
// purged while a legitimately long wait could still plausibly be pending.
const MaxDelayDeadlineAge = 400 * 24 * time.Hour

// DelayDeadlineRepo persists the resolved fire-at instant for a relative
// dynamic delay (delay.dynamic, dynamicKind="relative"), keyed by
// (run_id, step_id). See migrations/0031_delay_deadlines.sql for why this is
// a dedicated table rather than step_outputs.
type DelayDeadlineRepo interface {
	// Get returns the persisted deadline for (runID, stepID), or ErrNotFound
	// if none has been written yet -- the caller's cue that this is the first
	// dispatch and it must resolve and Save one. Unscoped by design, mirroring
	// StepOutputs().ListByRun: the delay worker holds only the opaque run id
	// (already globally unique per notification), not a tenancy context.
	Get(ctx context.Context, runID, stepID string) (time.Time, error)

	// Save persists deadline for (runID, stepID). Written once, on first
	// dispatch: ON CONFLICT DO NOTHING so a later call (a duplicate dispatch
	// racing the first, or a caller that forgot to Get first) can never
	// overwrite an already-persisted deadline with a freshly resolved one --
	// that overwrite is exactly the bug this table exists to prevent.
	Save(ctx context.Context, scope domain.Scope, runID, stepID string, deadline time.Time) error

	// Clear deletes the row for (runID, stepID). Called on normal step
	// completion so the common path does not accumulate rows; retention is
	// only a backstop for the abandoned-run case. A missing row is a no-op
	// (the absolute delay forms never write one).
	Clear(ctx context.Context, runID, stepID string) error

	// PurgeOlderThan deletes rows created before cutoff and reports how many
	// were removed, mirroring StepOutputRepo.PurgeOlderThan. It is the
	// backstop for deadlines abandoned by a run that never reaches Clear,
	// driven by the boot-plus-ticker retention loop in cmd/notify rather than
	// piggybacked on the write path -- see MaxDelayDeadlineAge for the cutoff
	// rationale.
	PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

type delayDeadlineRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *delayDeadlineRepo) Get(ctx context.Context, runID, stepID string) (time.Time, error) {
	var raw string
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT deadline FROM delay_deadlines WHERE run_id = ? AND step_id = ?`,
		runID, stepID,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, err
	}
	return timestamp.Parse(raw)
}

func (repo *delayDeadlineRepo) Save(ctx context.Context, scope domain.Scope, runID, stepID string, deadline time.Time) error {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO delay_deadlines (org_id, environment_id, run_id, step_id, deadline, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id, step_id) DO NOTHING`,
		scope.OrgID, scope.EnvID, runID, stepID, timestamp.Format(deadline), timestamp.Now(),
	)
	return err
}

func (repo *delayDeadlineRepo) Clear(ctx context.Context, runID, stepID string) error {
	_, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM delay_deadlines WHERE run_id = ? AND step_id = ?`,
		runID, stepID,
	)
	return err
}

// PurgeOlderThan deletes delay_deadlines rows created before cutoff.
//
// This table would otherwise grow without bound once relative dynamic delays
// stop being created: Clear removes a row on normal completion, but an
// abandoned run (crashed or cancelled before Clear) leaves its row behind
// forever with no other mechanism to reclaim it.
//
// The comparison is a plain string range, which is only correct because
// created_at is written through the fixed-width timestamp layout -- see
// StepOutputRepo.PurgeOlderThan for the same caveat.
func (repo *delayDeadlineRepo) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM delay_deadlines WHERE created_at < ?`,
		timestamp.Format(cutoff),
	)
	if err != nil {
		return 0, fmt.Errorf("store: purge delay_deadlines: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge delay_deadlines rows affected: %w", err)
	}
	return n, nil
}
