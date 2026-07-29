package store_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// step_outputs holds captured HTTP response bodies and previously grew without
// bound. These pin the retention purge, and in particular that it removes ONLY
// what it should: a purge that over-deletes silently strips a running workflow
// of the upstream outputs its downstream steps interpolate.

func purgeTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestPurgeOlderThan_RemovesOnlyOldRows is the core contract.
func TestPurgeOlderThan_RemovesOnlyOldRows(t *testing.T) {
	ctx := context.Background()
	st := purgeTestStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if err := st.StepOutputs().Save(ctx, scope, "run_recent", "s1", json.RawMessage(`{"statusCode":200}`)); err != nil {
		t.Fatalf("Save recent: %v", err)
	}

	// A cutoff in the past cannot match a row just written, so nothing goes.
	n, err := st.StepOutputs().PurgeOlderThan(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if n != 0 {
		t.Errorf("purged %d row(s) with a past cutoff, want 0", n)
	}
	if got, err := st.StepOutputs().ListByRun(ctx, "run_recent"); err != nil || len(got) != 1 {
		t.Fatalf("recent row was removed: got %v err %v", got, err)
	}

	// A cutoff in the future matches everything.
	n, err = st.StepOutputs().PurgeOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeOlderThan future: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d row(s) with a future cutoff, want 1", n)
	}
	if got, _ := st.StepOutputs().ListByRun(ctx, "run_recent"); len(got) != 0 {
		t.Errorf("row survived a cutoff after its creation: %v", got)
	}
}

// TestPurgeOlderThan_EmptyTable guards the ordinary steady state: a purge with
// nothing to do must succeed quietly, since it runs on a timer forever.
func TestPurgeOlderThan_EmptyTable(t *testing.T) {
	st := purgeTestStore(t)
	n, err := st.StepOutputs().PurgeOlderThan(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("PurgeOlderThan on empty table: %v", err)
	}
	if n != 0 {
		t.Errorf("purged %d row(s) from an empty table", n)
	}
}

// TestPurgeOlderThan_IsCrossTenant pins that retention is system-wide. Purging
// per-tenant would leave any tenant the sweep missed growing unbounded, which
// is the problem this exists to solve.
func TestPurgeOlderThan_IsCrossTenant(t *testing.T) {
	ctx := context.Background()
	st := purgeTestStore(t)

	for _, sc := range []domain.Scope{
		{OrgID: "org_1", EnvID: "env_1"},
		{OrgID: "org_2", EnvID: "env_2"},
	} {
		if err := st.StepOutputs().Save(ctx, sc, "run_"+sc.OrgID, "s1", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("Save %s: %v", sc.OrgID, err)
		}
	}

	n, err := st.StepOutputs().PurgeOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if n != 2 {
		t.Errorf("purged %d row(s) across two tenants, want 2", n)
	}
}
