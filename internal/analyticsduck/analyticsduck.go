//go:build duckdb

// Package analyticsduck provides a DuckDB-backed implementation of
// store.AnalyticsRepo: a columnar OLAP sink + query source for delivery-status
// analytics. It is an opt-in backend, compiled only under the `duckdb` build
// tag (it links DuckDB's CGo static library); the default notify build stays
// pure-Go on the SQLite AnalyticsRepo. Wire it via analytics.NewWithRepo.
package analyticsduck

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// Repo is a DuckDB-backed store.AnalyticsRepo. DuckDB is an embedded columnar
// engine: one handle serves both appends and aggregations, so a single *sql.DB
// suffices — no writer/reader split like SQLite's single-writer WAL pools.
type Repo struct {
	db *sql.DB
}

// schema mirrors the SQLite analytics_deliveries table column-for-column so the
// two backends are query-compatible; created_at stays an RFC3339Nano string for
// parity with the SQLite repo rather than a native TIMESTAMP.
const schema = `CREATE TABLE IF NOT EXISTS analytics_deliveries (
	id             VARCHAR PRIMARY KEY,
	org_id         VARCHAR NOT NULL,
	environment_id VARCHAR NOT NULL,
	channel        VARCHAR NOT NULL,
	status         VARCHAR NOT NULL,
	subscriber_id  VARCHAR,
	workflow_id    VARCHAR,
	transaction_id VARCHAR,
	created_at     VARCHAR NOT NULL
)`

// Open opens (or creates) a DuckDB database at path — pass "" for an in-memory
// database — and ensures the analytics schema exists. The returned *Repo
// satisfies store.AnalyticsRepo.
func Open(ctx context.Context, path string) (*Repo, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("analyticsduck: open %q: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("analyticsduck: schema: %w", err)
	}
	return &Repo{db: db}, nil
}

// Close closes the underlying DuckDB handle.
func (r *Repo) Close() error { return r.db.Close() }

// Insert appends one delivery observation, minting id (and created_at) when
// empty and forcing the row's scope to the caller's — identical semantics to
// the SQLite AnalyticsRepo.
func (r *Repo) Insert(ctx context.Context, scope domain.Scope, d domain.AnalyticsDelivery) error {
	d.OrgID = scope.OrgID
	d.EnvID = scope.EnvID
	if d.ID == "" {
		d.ID = store.NewULID()
	}
	if d.CreatedAt == "" {
		d.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO analytics_deliveries
		   (id, org_id, environment_id, channel, status, subscriber_id,
		    workflow_id, transaction_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.OrgID, d.EnvID, d.Channel, d.Status, d.SubscriberID,
		d.WorkflowID, d.TransactionID, d.CreatedAt,
	)
	return err
}

// CountByChannelStatus returns counts grouped by channel then status, filtered
// to scope — identical result shape to the SQLite AnalyticsRepo.
func (r *Repo) CountByChannelStatus(ctx context.Context, scope domain.Scope) (map[string]map[string]int, error) {
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT channel, status, COUNT(*)
		 FROM analytics_deliveries
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

// DeliveriesByDay returns ordered per-day delivery counts grouped by
// (day, channel, status), scope-filtered. It delegates to the shared
// store.QueryDeliveriesByDay so the DuckDB backend answers the over-time query
// identically to the SQLite repo.
func (r *Repo) DeliveriesByDay(ctx context.Context, scope domain.Scope) ([]domain.DeliveryDayCount, error) {
	return store.QueryDeliveriesByDay(ctx, r.db, scope)
}
