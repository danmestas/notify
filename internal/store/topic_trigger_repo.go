package store

import (
	"context"
	"database/sql"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// TopicTriggerRepo persists the (transaction_id, topic_key) association
// written at multicast trigger time. Every method is Scope-first.
// The table enables the Topic Activity Feed to reconstruct per-topic delivery
// lists by joining analytics_deliveries ON transaction_id (Option A,
// spec-04 §5 BP-AF-TOPIC).
type TopicTriggerRepo interface {
	// Record inserts one (transaction_id, topic_key) pair. Silently ignores
	// duplicate (environment_id, transaction_id) — idempotent re-play is safe.
	Record(ctx context.Context, scope domain.Scope, transactionID, topicKey string) error
	// ListByTopic returns activity rows for the topic, newest-first, up to
	// limit rows. Pass after="" for the first page; subsequent pages pass
	// the last row's ID as the cursor.
	ListByTopic(ctx context.Context, scope domain.Scope, topicKey, after string, limit int) (TopicActivityPage, error)
}

// TopicActivityRow is one row of topic broadcast activity, joining
// topic_triggers with analytics_deliveries via transaction_id.
type TopicActivityRow struct {
	// ID is the topic_triggers row id (used as cursor).
	ID            string
	TransactionID string
	TopicKey      string
	// Analytics fields from the joined delivery rows (may be zero-value
	// when no analytics delivery was recorded yet for this transaction).
	Channel      string
	Status       string
	SubscriberID string
	WorkflowID   string
	CreatedAt    string
}

// TopicActivityPage holds a page of activity rows plus a cursor for the next
// page (empty string when there are no more rows).
type TopicActivityPage struct {
	Items      []TopicActivityRow
	NextCursor string
}

type topicTriggerRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *topicTriggerRepo) Record(ctx context.Context, scope domain.Scope, transactionID, topicKey string) error {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO topic_triggers
		   (id, org_id, environment_id, topic_key, transaction_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, transaction_id) DO NOTHING`,
		newULID(), scope.OrgID, scope.EnvID, topicKey, transactionID,
		timestamp.Now(),
	)
	return err
}

func (repo *topicTriggerRepo) ListByTopic(ctx context.Context, scope domain.Scope, topicKey, after string, limit int) (TopicActivityPage, error) {
	if limit <= 0 {
		limit = 25
	}
	// Fetch one extra to detect whether a next page exists.
	fetch := limit + 1

	var rows *sql.Rows
	var err error
	// ULIDs are monotonically increasing (lexicographically) over time, so
	// ORDER BY id DESC is equivalent to newest-first and id < cursor is a
	// correct keyset cursor for this sort order.
	if after == "" {
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT tt.id, tt.transaction_id, tt.topic_key,
			        COALESCE(ad.channel, '')       AS channel,
			        COALESCE(ad.status, '')        AS status,
			        COALESCE(ad.subscriber_id, '') AS subscriber_id,
			        COALESCE(ad.workflow_id, '')   AS workflow_id,
			        tt.created_at
			 FROM topic_triggers tt
			 LEFT JOIN analytics_deliveries ad
			       ON ad.environment_id = tt.environment_id
			      AND ad.transaction_id = tt.transaction_id
			 WHERE tt.org_id = ? AND tt.environment_id = ? AND tt.topic_key = ?
			 ORDER BY tt.id DESC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, topicKey, fetch,
		)
	} else {
		// Cursor pagination: rows whose id < after (DESC order).
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT tt.id, tt.transaction_id, tt.topic_key,
			        COALESCE(ad.channel, '')       AS channel,
			        COALESCE(ad.status, '')        AS status,
			        COALESCE(ad.subscriber_id, '') AS subscriber_id,
			        COALESCE(ad.workflow_id, '')   AS workflow_id,
			        tt.created_at
			 FROM topic_triggers tt
			 LEFT JOIN analytics_deliveries ad
			       ON ad.environment_id = tt.environment_id
			      AND ad.transaction_id = tt.transaction_id
			 WHERE tt.org_id = ? AND tt.environment_id = ? AND tt.topic_key = ?
			   AND tt.id < ?
			 ORDER BY tt.id DESC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, topicKey, after, fetch,
		)
	}
	if err != nil {
		return TopicActivityPage{}, err
	}
	defer func() { _ = rows.Close() }()

	var items []TopicActivityRow
	for rows.Next() {
		var row TopicActivityRow
		if err := rows.Scan(
			&row.ID, &row.TransactionID, &row.TopicKey,
			&row.Channel, &row.Status, &row.SubscriberID, &row.WorkflowID,
			&row.CreatedAt,
		); err != nil {
			return TopicActivityPage{}, err
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return TopicActivityPage{}, err
	}

	var nextCursor string
	if len(items) > limit {
		nextCursor = items[limit-1].ID
		items = items[:limit]
	}
	return TopicActivityPage{Items: items, NextCursor: nextCursor}, nil
}
