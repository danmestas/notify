//go:build duckdb

package analyticsduck

import (
	"context"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/domain"
)

// ExportDeliveriesParquet writes scope's analytics_deliveries rows to a Parquet
// file at path using DuckDB's native columnar writer (built into core; no
// extension). This is a portable columnar extract — for a data lake, external
// BI, or archival — that the SQLite backend cannot produce.
//
// scope.OrgID/EnvID and path are trusted server-controlled values, not user
// input; single quotes are escaped defensively. COPY does not accept bind
// parameters in its target subquery, so the scope filter is inlined.
func (r *Repo) ExportDeliveriesParquet(ctx context.Context, scope domain.Scope, path string) error {
	esc := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	q := fmt.Sprintf(
		`COPY (SELECT id, org_id, environment_id, channel, status, subscriber_id,
		         workflow_id, transaction_id, created_at
		       FROM analytics_deliveries
		       WHERE org_id = '%s' AND environment_id = '%s'
		       ORDER BY created_at) TO '%s' (FORMAT PARQUET)`,
		esc(scope.OrgID), esc(scope.EnvID), esc(path),
	)
	if _, err := r.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("analyticsduck: export parquet %q: %w", path, err)
	}
	return nil
}
