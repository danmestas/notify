//go:build duckdb

package analyticsduck

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/marcboeker/go-duckdb/v2"

	"github.com/danmestas/notify/internal/domain"
)

// AttachedCountByChannel uses DuckDB's sqlite extension to query the SQLite OLTP
// database at sqlitePath directly — read-only and zero-copy, with no projection
// step — returning delivery counts grouped by channel then status for scope.
//
// This is DuckDB's cross-engine access pattern: run OLAP over the operational
// source of truth without an ETL/projection table. It complements the projector
// path (which copies delivery events into DuckDB's own table); ATTACH instead
// reads notify's live SQLite tables in place.
//
// sqlitePath is a trusted server path, not user input; single quotes are escaped
// defensively. The caller should ensure SQLite has checkpointed its WAL (e.g.
// the writer pool is closed) so recently written rows are visible to the reader.
func AttachedCountByChannel(ctx context.Context, sqlitePath string, scope domain.Scope) (map[string]map[string]int, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("analyticsduck: open duckdb: %w", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(ctx, `INSTALL sqlite`); err != nil {
		return nil, fmt.Errorf("analyticsduck: install sqlite extension: %w", err)
	}
	if _, err := db.ExecContext(ctx, `LOAD sqlite`); err != nil {
		return nil, fmt.Errorf("analyticsduck: load sqlite extension: %w", err)
	}
	attach := fmt.Sprintf(
		`ATTACH '%s' AS oltp (TYPE sqlite, READ_ONLY)`,
		strings.ReplaceAll(sqlitePath, "'", "''"),
	)
	if _, err := db.ExecContext(ctx, attach); err != nil {
		return nil, fmt.Errorf("analyticsduck: attach sqlite %q: %w", sqlitePath, err)
	}
	defer func() { _, _ = db.ExecContext(ctx, `DETACH oltp`) }()

	rows, err := db.QueryContext(
		ctx,
		`SELECT channel, status, COUNT(*)
		 FROM oltp.analytics_deliveries
		 WHERE org_id = ? AND environment_id = ?
		 GROUP BY channel, status`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]map[string]int)
	for rows.Next() {
		var (
			channel string
			status  string
			n       int
		)
		if err := rows.Scan(&channel, &status, &n); err != nil {
			return nil, err
		}
		byStatus := out[channel]
		if byStatus == nil {
			byStatus = make(map[string]int)
			out[channel] = byStatus
		}
		byStatus[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
