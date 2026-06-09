package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// AnalyticsRepo is the analytics projector's sink and query source. Insert
// appends one delivery observation; CountByChannelStatus aggregates them for
// the query API. Every method is Scope-first (org_id + environment_id).
type AnalyticsRepo interface {
	// Insert appends a delivery observation, minting id (and created_at) when empty.
	Insert(ctx context.Context, scope domain.Scope, d domain.AnalyticsDelivery) error
	// CountByChannelStatus returns counts grouped by channel then status, scope-filtered.
	CountByChannelStatus(ctx context.Context, scope domain.Scope) (map[string]map[string]int, error)
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

func (repo *analyticsRepo) CountByChannelStatus(ctx context.Context, scope domain.Scope) (map[string]map[string]int, error) {
	rows, err := repo.r.QueryContext(
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
