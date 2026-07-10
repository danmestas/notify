package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// AnalyticsFilter holds optional filter parameters for analytics aggregation
// queries. Zero value means "no extra filter" — all rows for the scope.
type AnalyticsFilter struct {
	// WorkflowID, when non-empty, restricts the query to rows whose workflow_id
	// matches this value. Corresponds to the Wave-4 workflow filter pill (GAP-AF-06).
	WorkflowID string
}

// AnalyticsRepo is the analytics projector's sink and query source. Insert
// appends one delivery observation; CountByChannelStatus aggregates them for
// the query API. Every method is Scope-first (org_id + environment_id).
type AnalyticsRepo interface {
	// Insert appends a delivery observation, minting id (and created_at) when empty.
	Insert(ctx context.Context, scope domain.Scope, d domain.AnalyticsDelivery) error
	// CountByChannelStatus returns counts grouped by channel then status, scope-filtered.
	// The optional filter may restrict results to a specific workflow_id.
	CountByChannelStatus(ctx context.Context, scope domain.Scope, f AnalyticsFilter) (map[string]map[string]int, error)
	// DeliveriesByDay returns ordered per-day delivery counts grouped by
	// (day, channel, status), scope-filtered — the analytics over-time series.
	// The optional filter may restrict results to a specific workflow_id.
	DeliveriesByDay(ctx context.Context, scope domain.Scope, f AnalyticsFilter) ([]domain.DeliveryDayCount, error)
	// CountActiveSubscribers returns the number of distinct subscriber_ids that
	// appear in the analytics_deliveries table for the given scope and since
	// cutoff. The optional filter may restrict results to a specific
	// workflow_id (GAP-AF-05, M1).
	CountActiveSubscribers(ctx context.Context, scope domain.Scope, since time.Time, f AnalyticsFilter) (int64, error)
	// AverageMessagesPerSubscriber returns the average number of delivery rows
	// per distinct subscriber for the given scope and since cutoff. Returns 0
	// when there are no subscribers. The optional filter may restrict results
	// to a specific workflow_id (GAP-AF-05, M1).
	AverageMessagesPerSubscriber(ctx context.Context, scope domain.Scope, since time.Time, f AnalyticsFilter) (float64, error)
}

type analyticsRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *analyticsRepo) Insert(ctx context.Context, scope domain.Scope, d domain.AnalyticsDelivery) error {
	d.OrgID = scope.OrgID
	d.EnvID = scope.EnvID
	if d.ID == "" {
		d.ID = newULID()
	}
	if d.CreatedAt == "" {
		d.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := repo.w.ExecContext(
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

func (repo *analyticsRepo) CountByChannelStatus(ctx context.Context, scope domain.Scope, f AnalyticsFilter) (map[string]map[string]int, error) {
	return QueryCountByChannelStatus(ctx, repo.r, scope, f)
}

// QueryCountByChannelStatus runs the channel/status aggregation against db. It
// is shared by the SQLite repo and the DuckDB backend so both implementations
// use one source of truth. The optional AnalyticsFilter may add a workflow_id
// predicate (GAP-AF-06).
func QueryCountByChannelStatus(ctx context.Context, db *sql.DB, scope domain.Scope, f AnalyticsFilter) (map[string]map[string]int, error) {
	q := `SELECT channel, status, COUNT(*)
	      FROM analytics_deliveries
	      WHERE org_id = ? AND environment_id = ?`
	args := []any{scope.OrgID, scope.EnvID}
	if f.WorkflowID != "" {
		q += ` AND workflow_id = ?`
		args = append(args, f.WorkflowID)
	}
	q += ` GROUP BY channel, status`

	rows, err := db.QueryContext(ctx, q, args...)
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

func (repo *analyticsRepo) DeliveriesByDay(ctx context.Context, scope domain.Scope, f AnalyticsFilter) ([]domain.DeliveryDayCount, error) {
	return QueryDeliveriesByDay(ctx, repo.r, scope, f)
}

// QueryDeliveriesByDay runs the delivery over-time query against db and returns
// ordered per-day (channel, status) counts. It is shared by the SQLite repo and
// the DuckDB analytics backend so both answer the query from one source of
// truth; substr(created_at, 1, 10) buckets the RFC3339Nano timestamp by UTC day
// and is valid in both SQLite and DuckDB. The optional AnalyticsFilter may add
// a workflow_id predicate (GAP-AF-06).
func QueryDeliveriesByDay(ctx context.Context, db *sql.DB, scope domain.Scope, f AnalyticsFilter) ([]domain.DeliveryDayCount, error) {
	q := `SELECT substr(created_at, 1, 10) AS day, channel, status, COUNT(*)
	      FROM analytics_deliveries
	      WHERE org_id = ? AND environment_id = ?`
	args := []any{scope.OrgID, scope.EnvID}
	if f.WorkflowID != "" {
		q += ` AND workflow_id = ?`
		args = append(args, f.WorkflowID)
	}
	q += ` GROUP BY day, channel, status ORDER BY day, channel, status`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.DeliveryDayCount
	for rows.Next() {
		var c domain.DeliveryDayCount
		if err := rows.Scan(&c.Day, &c.Channel, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (repo *analyticsRepo) CountActiveSubscribers(ctx context.Context, scope domain.Scope, since time.Time, f AnalyticsFilter) (int64, error) {
	return QueryCountActiveSubscribers(ctx, repo.r, scope, since, f)
}

// QueryCountActiveSubscribers counts distinct subscriber_ids that have at least
// one delivery row at or after since for the given scope. Shared by the SQLite
// and DuckDB backends. The optional AnalyticsFilter may add a workflow_id
// predicate (GAP-AF-05, M1).
func QueryCountActiveSubscribers(ctx context.Context, db *sql.DB, scope domain.Scope, since time.Time, f AnalyticsFilter) (int64, error) {
	// Empty subscriber_id rows (legacy/anonymous deliveries) are excluded by the
	// WHERE subscriber_id != '' guard so they don't inflate the count.
	q, args := buildSubscriberQuery(scope, since, f)
	q = fmt.Sprintf(`SELECT COUNT(DISTINCT subscriber_id) FROM (%s)`, q)
	var n int64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (repo *analyticsRepo) AverageMessagesPerSubscriber(ctx context.Context, scope domain.Scope, since time.Time, f AnalyticsFilter) (float64, error) {
	return QueryAverageMessagesPerSubscriber(ctx, repo.r, scope, since, f)
}

// QueryAverageMessagesPerSubscriber returns the average number of delivery rows
// per distinct (non-empty) subscriber_id for the given scope and since cutoff.
// Returns 0.0 when there are no rows. Shared by SQLite and DuckDB. The optional
// AnalyticsFilter may add a workflow_id predicate (GAP-AF-05, M1).
func QueryAverageMessagesPerSubscriber(ctx context.Context, db *sql.DB, scope domain.Scope, since time.Time, f AnalyticsFilter) (float64, error) {
	q, args := buildSubscriberQuery(scope, since, f)
	// Compute total rows and distinct-subscriber count in one pass: avg = total/distinct.
	// CAST to REAL guards against integer division in SQLite.
	outer := fmt.Sprintf(
		`SELECT CAST(COUNT(*) AS REAL), COUNT(DISTINCT subscriber_id) FROM (%s)`,
		q,
	)
	var total, distinct int64
	if err := db.QueryRowContext(ctx, outer, args...).Scan(&total, &distinct); err != nil {
		return 0, err
	}
	if distinct == 0 {
		return 0, nil
	}
	return float64(total) / float64(distinct), nil
}

// buildSubscriberQuery returns the inner SELECT used by both CountActiveSubscribers
// and AverageMessagesPerSubscriber: all rows for the scope/since window where
// subscriber_id is non-empty. The optional AnalyticsFilter.WorkflowID adds a
// workflow_id predicate so both aggregations can scope to a single workflow
// (M1: previously these two queries silently ignored the workflow filter).
func buildSubscriberQuery(scope domain.Scope, since time.Time, f AnalyticsFilter) (string, []any) {
	var sb strings.Builder
	sb.WriteString(
		`SELECT subscriber_id FROM analytics_deliveries
		 WHERE org_id = ? AND environment_id = ?
		   AND subscriber_id IS NOT NULL AND subscriber_id != ''`,
	)
	args := []any{scope.OrgID, scope.EnvID}
	if !since.IsZero() {
		sb.WriteString(` AND created_at >= ?`)
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if f.WorkflowID != "" {
		sb.WriteString(` AND workflow_id = ?`)
		args = append(args, f.WorkflowID)
	}
	return sb.String(), args
}
