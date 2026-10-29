package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

func TestMigrate_0031_AppliesDelayDeadlines(t *testing.T) {
	s := openTemp(t)
	if n := migrationCount(t, s); n != 43 {
		t.Fatalf("expected 35 recorded migrations, got %d", n)
	}
	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='delay_deadlines'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("delay_deadlines table missing: %v", err)
	}
}

// TestDelayDeadlineRepo_GetMissingReturnsNotFound proves the first-dispatch
// signal: no row yet means ErrNotFound, which callers read as "resolve and
// Save one".
func TestDelayDeadlineRepo_GetMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	_, err := s.DelayDeadlines().Get(ctx, "run_1", "step_1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

// TestDelayDeadlineRepo_SaveThenGet proves a saved deadline round-trips
// through the RFC3339 TEXT column unchanged (comparing via Equal absorbs any
// monotonic-reading difference on the input time.Time).
func TestDelayDeadlineRepo_SaveThenGet(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	deadline := time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)
	if err := s.DelayDeadlines().Save(ctx, scope, "run_1", "step_1", deadline); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.DelayDeadlines().Get(ctx, "run_1", "step_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Equal(deadline) {
		t.Fatalf("Get = %v, want %v", got, deadline)
	}
}

// TestDelayDeadlineRepo_SaveDoesNotOverwriteExisting is the correctness
// invariant the whole fix depends on: once a deadline is persisted, a second
// Save for the same (run_id, step_id) — e.g. a requeue re-resolving "now" —
// must NOT clobber it. Overwriting here is exactly the bug #273 fixes.
func TestDelayDeadlineRepo_SaveDoesNotOverwriteExisting(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	original := time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)
	if err := s.DelayDeadlines().Save(ctx, scope, "run_1", "step_1", original); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	later := original.Add(24 * time.Hour)
	if err := s.DelayDeadlines().Save(ctx, scope, "run_1", "step_1", later); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := s.DelayDeadlines().Get(ctx, "run_1", "step_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Equal(original) {
		t.Fatalf("Get = %v, want original %v (second Save must not overwrite)", got, original)
	}
}

// TestDelayDeadlineRepo_Clear proves Clear deletes the row so the common
// (normal-completion) path does not accumulate rows.
func TestDelayDeadlineRepo_Clear(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := s.DelayDeadlines().Save(ctx, scope, "run_1", "step_1", time.Now()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.DelayDeadlines().Clear(ctx, "run_1", "step_1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if _, err := s.DelayDeadlines().Get(ctx, "run_1", "step_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Clear = %v, want ErrNotFound", err)
	}
}

// TestDelayDeadlineRepo_ClearMissingIsNoop proves clearing a never-written
// row (the instant/scheduled absolute forms, which write none) is harmless.
func TestDelayDeadlineRepo_ClearMissingIsNoop(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	if err := s.DelayDeadlines().Clear(ctx, "no-such-run", "no-such-step"); err != nil {
		t.Fatalf("Clear missing row: %v", err)
	}
}

// TestDelayDeadlineRepo_SurvivesPastStepOutputsRetentionWindow proves the
// specific claim #273 exists to fix: a deadline for a delay far longer than
// step_outputs' 7-day retention window is still readable once its row is
// older than 7 days, because delay_deadlines carries no such purge.
func TestDelayDeadlineRepo_SurvivesPastStepOutputsRetentionWindow(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	deadline := time.Now().Add(60 * 24 * time.Hour) // a 60-day relative delay
	if err := s.DelayDeadlines().Save(ctx, scope, "run_long", "step_1", deadline); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Backdate created_at to 10 days ago -- past step_outputs' 7-day window --
	// directly against the writer pool, simulating time having passed.
	backdated := timestamp.Format(time.Now().Add(-10 * 24 * time.Hour))
	if _, err := s.writer.ExecContext(
		ctx,
		`UPDATE delay_deadlines SET created_at = ? WHERE run_id = ? AND step_id = ?`,
		backdated, "run_long", "step_1",
	); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}

	got, err := s.DelayDeadlines().Get(ctx, "run_long", "step_1")
	if err != nil {
		t.Fatalf("Get after 10 days: %v, want the deadline still present", err)
	}
	if !got.Equal(deadline) {
		t.Fatalf("Get = %v, want %v", got, deadline)
	}
}

// TestDelayDeadlineRepo_PurgeOlderThanSweepsExpiredRows proves the
// background-purge mechanism: PurgeOlderThan deletes rows older than the
// given cutoff (abandoned by a run that never reached Clear) but leaves
// rows within it untouched. This is the retention path startDelayDeadlinePurge
// drives from cmd/notify/retention.go -- Save itself no longer sweeps.
func TestDelayDeadlineRepo_PurgeOlderThanSweepsExpiredRows(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := s.DelayDeadlines().Save(ctx, scope, "run_old", "step_1", time.Now()); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := s.DelayDeadlines().Save(ctx, scope, "run_recent", "step_1", time.Now()); err != nil {
		t.Fatalf("seed recent: %v", err)
	}

	expired := timestamp.Format(time.Now().Add(-(MaxDelayDeadlineAge + 24*time.Hour)))
	if _, err := s.writer.ExecContext(
		ctx,
		`UPDATE delay_deadlines SET created_at = ? WHERE run_id = ?`, expired, "run_old",
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cutoff := time.Now().Add(-MaxDelayDeadlineAge)
	n, err := s.DelayDeadlines().PurgeOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if n != 1 {
		t.Fatalf("PurgeOlderThan rows = %d, want 1", n)
	}

	if _, err := s.DelayDeadlines().Get(ctx, "run_old", "step_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run_old survived the purge: err = %v, want ErrNotFound", err)
	}
	if _, err := s.DelayDeadlines().Get(ctx, "run_recent", "step_1"); err != nil {
		t.Fatalf("run_recent wrongly purged: %v", err)
	}
}

// TestDelayDeadlineRepo_SaveDoesNotPurgeExpiredRows proves Save is a pure
// write path now: an expired row left over from before this change is not
// touched by a subsequent Save. Only PurgeOlderThan (driven by the
// boot-plus-ticker retention loop) removes expired rows.
func TestDelayDeadlineRepo_SaveDoesNotPurgeExpiredRows(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := s.DelayDeadlines().Save(ctx, scope, "run_old", "step_1", time.Now()); err != nil {
		t.Fatalf("seed old: %v", err)
	}

	expired := timestamp.Format(time.Now().Add(-(MaxDelayDeadlineAge + 24*time.Hour)))
	if _, err := s.writer.ExecContext(
		ctx,
		`UPDATE delay_deadlines SET created_at = ? WHERE run_id = ?`, expired, "run_old",
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if err := s.DelayDeadlines().Save(ctx, scope, "run_trigger", "step_1", time.Now()); err != nil {
		t.Fatalf("unrelated Save: %v", err)
	}

	if _, err := s.DelayDeadlines().Get(ctx, "run_old", "step_1"); err != nil {
		t.Fatalf("run_old wrongly swept by Save: %v, want nil (Save must not delete)", err)
	}
}
