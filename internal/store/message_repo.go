package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// MessageRepo persists per-channel messages and serves the inbox list keyed by
// (environment_id, subscriber_id, channel) — backed by the msg_inbox index.
// Scope-first throughout.
type MessageRepo interface {
	Create(ctx context.Context, scope domain.Scope, m domain.Message) (domain.Message, error)
	List(ctx context.Context, scope domain.Scope, subscriberID, channel string) ([]domain.Message, error)
	// ListByIntegrationID returns messages for the given integration, ordered
	// newest-first. Capped at limit rows (1–100; 0 defaults to 100). Rows
	// written before migration 0017 (integration_id='') are excluded.
	// Scope-first: org_id + environment_id must match the integration's scope.
	ListByIntegrationID(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]domain.Message, error)
	// ListByNotificationID returns all messages for a notification, ordered
	// newest-first. Scope-first.
	ListByNotificationID(ctx context.Context, scope domain.Scope, notificationID string) ([]domain.Message, error)
}

type messageRepo struct {
	w *sql.DB
	r *sql.DB
}

// messageColumns is the canonical ordered column list for SELECT and INSERT.
// Keeping it in one place means adding a column only requires updating this
// constant and the corresponding Scan call.
const messageColumns = `id, org_id, environment_id, workflow_id, notification_id, run_id, step_id,
	subscriber_id, channel, provider_id, integration_id, content, subject, status, seen, read,
	archived, transaction_id, error_text, created_at`

func scanMessage(rows *sql.Rows) (domain.Message, error) {
	var m domain.Message
	err := rows.Scan(
		&m.ID, &m.OrgID, &m.EnvID, &m.WorkflowID, &m.NotificationID, &m.RunID, &m.StepID,
		&m.SubscriberID, &m.Channel, &m.ProviderID, &m.IntegrationID, &m.Content, &m.Subject,
		&m.Status, &m.Seen, &m.Read, &m.Archived, &m.TransactionID, &m.ErrorText, &m.CreatedAt,
	)
	return m, err
}

func (repo *messageRepo) Create(ctx context.Context, scope domain.Scope, m domain.Message) (domain.Message, error) {
	m.OrgID = scope.OrgID
	m.EnvID = scope.EnvID
	if m.ID == "" {
		m.ID = newULID()
	}
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO messages
		   (id, org_id, environment_id, workflow_id, notification_id, run_id, step_id, subscriber_id,
		    channel, provider_id, integration_id, content, subject, status, seen, read, archived,
		    transaction_id, error_text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OrgID, m.EnvID, m.WorkflowID, m.NotificationID, m.RunID, m.StepID, m.SubscriberID,
		m.Channel, m.ProviderID, m.IntegrationID, m.Content, m.Subject, m.Status, m.Seen, m.Read,
		m.Archived, m.TransactionID, m.ErrorText, m.CreatedAt,
	)
	if err != nil {
		return domain.Message{}, err
	}
	return m, nil
}

func (repo *messageRepo) List(ctx context.Context, scope domain.Scope, subscriberID, channel string) ([]domain.Message, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND channel = ?
		 ORDER BY created_at DESC, id DESC`,
		scope.OrgID, scope.EnvID, subscriberID, channel,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListByIntegrationID returns up to limit messages for the given integration,
// ordered newest-first (created_at DESC, id DESC). Rows from before migration
// 0017 (integration_id=”) are excluded by the non-empty constraint on the
// queried integration_id. Limit is clamped to [1, 100]; 0 → 100.
func (repo *messageRepo) ListByIntegrationID(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`,
		scope.OrgID, scope.EnvID, integrationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListByNotificationID returns all messages for a notification, ordered
// newest-first. Used by GET /v1/notifications/{id}/messages (#43).
func (repo *messageRepo) ListByNotificationID(ctx context.Context, scope domain.Scope, notificationID string) ([]domain.Message, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND notification_id = ?
		 ORDER BY created_at DESC, id DESC`,
		scope.OrgID, scope.EnvID, notificationID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
