//go:build duckdb

package analyticsduck

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// TestAttachedCountByChannel_OverSQLite proves DuckDB can run OLAP directly over
// notify's live SQLite OLTP database via the sqlite extension + ATTACH, with no
// projection table: rows written through the SQLite AnalyticsRepo are counted by
// DuckDB reading the same .db file in place, scope-filtered, with the expected
// results. Real SQLite store (migrations + WAL) + real DuckDB ATTACH, no mocks.
func TestAttachedCountByChannel_OverSQLite(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "notify.db")

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	for i, r := range []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "sent"},
	} {
		if err := st.Analytics().Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// A row under a different environment must not leak through the ATTACH query.
	if err := st.Analytics().Insert(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"},
		domain.AnalyticsDelivery{Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("insert isolation: %v", err)
	}
	// Close so the WAL is checkpointed into the main db file DuckDB reads.
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	got, err := AttachedCountByChannel(ctx, dbPath, scope)
	if err != nil {
		t.Fatalf("AttachedCountByChannel: %v", err)
	}
	if got["in_app"]["sent"] != 2 || got["email"]["sent"] != 1 {
		t.Fatalf("attach counts = %v, want in_app/sent=2 email/sent=1", got)
	}
	if len(got) != 2 {
		t.Fatalf("attach returned %d channels, want 2 (scope isolation?): %v", len(got), got)
	}
}
