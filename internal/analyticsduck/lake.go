//go:build duckdb

package analyticsduck

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/danmestas/notify/internal/analyticslake"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// LakeRepo is a store.AnalyticsRepo whose queries read notify's analytics
// lake (#119): DuckDB over the live parts of notify/stream_notify_analytics,
// with each row's fields taken from its JSON payload. Lode fills the table
// from NOTIFY_ANALYTICS; `notify analytics-import` fills it with the SQLite
// rows from before. Insert goes to the wrapped repo, because the lake is
// written by Lode, not by notify's projector.
//
// Before each query it lists the live parts and redefines an
// analytics_deliveries view over them with the SQLite table's columns, then
// runs the same shared store.Query* SQL as the SQLite and DuckDB repos, so
// the three answer alike. An event id seen twice (an event in both the import
// and the live stream, or two overlapping parts) counts once.
type LakeRepo struct {
	writes store.AnalyticsRepo
	cfg    analyticslake.Config
	db     *sql.DB
	mu     sync.Mutex // one view definition at a time: refresh and query together
}

// OpenLake opens an in-memory DuckDB over the lake cfg names. An R2 lake loads
// the httpfs extension (fetched once from DuckDB's extension repository if it
// is not installed) and registers cfg's credential as a DuckDB secret.
func OpenLake(ctx context.Context, cfg analyticslake.Config, writes store.AnalyticsRepo) (*LakeRepo, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("analyticsduck: OpenLake: no lake configured")
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("analyticsduck: open duckdb: %w", err)
	}
	// One connection holds the secret and the view.
	db.SetMaxOpenConns(1)
	if cfg.IsS3() {
		if err := s3Secret(ctx, db, cfg); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	repo := &LakeRepo{writes: writes, cfg: cfg, db: db}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.refresh(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

// s3Secret loads httpfs and registers the lake's credential. CREATE SECRET
// takes no bind parameters, so values are quoted literals; the statement is
// never logged or returned in an error.
func s3Secret(ctx context.Context, db *sql.DB, cfg analyticslake.Config) error {
	host, https, err := cfg.EndpointHost()
	if err != nil {
		return err
	}
	for _, stmt := range []string{`INSTALL httpfs`, `LOAD httpfs`} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("analyticsduck: %s: %w", stmt, err)
		}
	}
	secret := fmt.Sprintf(`CREATE OR REPLACE SECRET notify_lake (TYPE S3, KEY_ID %s, SECRET %s,
		ENDPOINT %s, REGION 'auto', URL_STYLE 'path', USE_SSL %t, SCOPE %s)`,
		quote(cfg.AccessKey), quote(cfg.SecretKey), quote(host), https,
		quote("s3://"+cfg.Bucket+"/"))
	if _, err := db.ExecContext(ctx, secret); err != nil {
		return fmt.Errorf("analyticsduck: create lake secret for %s failed", cfg)
	}
	return nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// emptyView has the view's columns and no rows, for a lake with no parts.
const emptyView = `CREATE OR REPLACE VIEW analytics_deliveries AS
	SELECT NULL::VARCHAR AS org_id, NULL::VARCHAR AS environment_id,
	       NULL::VARCHAR AS channel, NULL::VARCHAR AS status,
	       NULL::VARCHAR AS subscriber_id, NULL::VARCHAR AS workflow_id,
	       NULL::VARCHAR AS transaction_id, NULL::VARCHAR AS run_id,
	       NULL::VARCHAR AS created_at
	WHERE false`

// lakeView maps each payload's v1 fields to the SQLite columns, keeping one
// row per event_id. created_at is the payload's createdAt, the message's
// creation time, as in SQLite: day grouping and the since filter must never
// use dt, event_time or at, because imported rows partition by createdAt and
// live rows by publish time, so those would shift days at the import boundary.
// hive_partitioning is off so dt is not even a column here.
const lakeView = `CREATE OR REPLACE VIEW analytics_deliveries AS
	SELECT json_extract_string(body, '$.orgId')         AS org_id,
	       json_extract_string(body, '$.envId')         AS environment_id,
	       json_extract_string(body, '$.channel')       AS channel,
	       json_extract_string(body, '$.status')        AS status,
	       json_extract_string(body, '$.subscriberId')  AS subscriber_id,
	       json_extract_string(body, '$.workflowId')    AS workflow_id,
	       json_extract_string(body, '$.transactionId') AS transaction_id,
	       json_extract_string(body, '$.runId')         AS run_id,
	       json_extract_string(body, '$.createdAt')     AS created_at
	FROM (
	  SELECT decode(payload) AS body
	  FROM read_parquet([%s], hive_partitioning = false)
	  QUALIFY row_number() OVER (PARTITION BY event_id ORDER BY event_time, stream_seq) = 1
	)`

// refresh redefines the view over the lake's live parts. Callers hold mu.
func (r *LakeRepo) refresh(ctx context.Context) error {
	parts, err := r.cfg.LiveParts(ctx)
	if err != nil {
		return fmt.Errorf("analyticsduck: list lake parts: %w", err)
	}
	stmt := emptyView
	if len(parts) > 0 {
		quoted := make([]string, len(parts))
		for i, part := range parts {
			quoted[i] = quote(part)
		}
		stmt = fmt.Sprintf(lakeView, strings.Join(quoted, ", "))
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("analyticsduck: lake view: %w", err)
	}
	return nil
}

// query refreshes the view and runs fn against it.
func query[T any](ctx context.Context, r *LakeRepo, fn func(*sql.DB) (T, error)) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(ctx); err != nil {
		var zero T
		return zero, err
	}
	return fn(r.db)
}

// Close closes the DuckDB handle. The wrapped repo is the caller's.
func (r *LakeRepo) Close() error { return r.db.Close() }

// Insert appends to the wrapped repo; the lake is Lode's to write.
func (r *LakeRepo) Insert(ctx context.Context, scope domain.Scope, d domain.AnalyticsDelivery) error {
	return r.writes.Insert(ctx, scope, d)
}

// CountByChannelStatus answers store.QueryCountByChannelStatus from the lake.
func (r *LakeRepo) CountByChannelStatus(ctx context.Context, scope domain.Scope, f store.AnalyticsFilter) (map[string]map[string]int, error) {
	return query(ctx, r, func(db *sql.DB) (map[string]map[string]int, error) {
		return store.QueryCountByChannelStatus(ctx, db, scope, f)
	})
}

// DeliveriesByDay answers store.QueryDeliveriesByDay from the lake.
func (r *LakeRepo) DeliveriesByDay(ctx context.Context, scope domain.Scope, f store.AnalyticsFilter) ([]domain.DeliveryDayCount, error) {
	return query(ctx, r, func(db *sql.DB) ([]domain.DeliveryDayCount, error) {
		return store.QueryDeliveriesByDay(ctx, db, scope, f)
	})
}

// CountActiveSubscribers answers store.QueryCountActiveSubscribers from the lake.
func (r *LakeRepo) CountActiveSubscribers(ctx context.Context, scope domain.Scope, since time.Time, f store.AnalyticsFilter) (int64, error) {
	return query(ctx, r, func(db *sql.DB) (int64, error) {
		return store.QueryCountActiveSubscribers(ctx, db, scope, since, f)
	})
}

// AverageMessagesPerSubscriber answers store.QueryAverageMessagesPerSubscriber from the lake.
func (r *LakeRepo) AverageMessagesPerSubscriber(ctx context.Context, scope domain.Scope, since time.Time, f store.AnalyticsFilter) (float64, error) {
	return query(ctx, r, func(db *sql.DB) (float64, error) {
		return store.QueryAverageMessagesPerSubscriber(ctx, db, scope, since, f)
	})
}

// RunsByDay answers store.QueryRunsByDay from the lake.
func (r *LakeRepo) RunsByDay(ctx context.Context, scope domain.Scope, f store.AnalyticsFilter) ([]domain.RunDayCount, error) {
	return query(ctx, r, func(db *sql.DB) ([]domain.RunDayCount, error) {
		return store.QueryRunsByDay(ctx, db, scope, f)
	})
}
