//go:build duckdb

package analyticsduck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestRepo_ExportDeliveriesParquet proves the DuckDB columnar Parquet extract:
// scope's rows are written to a .parquet file (scope-filtered, so a foreign-env
// row is excluded) and the file reads back via DuckDB's read_parquet with the
// expected per-channel counts.
func TestRepo_ExportDeliveriesParquet(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = repo.Close() }()

	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	for i, r := range []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "sent"},
	} {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// A foreign-env row must not appear in the scoped export.
	if err := repo.Insert(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"},
		domain.AnalyticsDelivery{Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("insert isolation: %v", err)
	}

	path := filepath.Join(t.TempDir(), "deliveries.parquet")
	if err := repo.ExportDeliveriesParquet(ctx, scope, path); err != nil {
		t.Fatalf("ExportDeliveriesParquet: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("parquet file missing or empty: stat=%v err=%v", fi, err)
	}

	// Read the Parquet back through DuckDB and verify scope-only counts.
	rows, err := repo.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT channel, COUNT(*) FROM read_parquet('%s') GROUP BY channel ORDER BY channel`, path,
	))
	if err != nil {
		t.Fatalf("read_parquet: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int{}
	for rows.Next() {
		var ch string
		var n int
		if err := rows.Scan(&ch, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[ch] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got["in_app"] != 2 || got["email"] != 1 || len(got) != 2 {
		t.Fatalf("parquet contents = %v, want in_app=2 email=1 (scope-only)", got)
	}
}
