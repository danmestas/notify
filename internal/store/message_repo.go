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
}

type messageRepo struct {
	w *sql.DB
	r *sql.DB
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
		    channel, provider_id, content, subject, status, seen, read, archived, transaction_id, error_text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OrgID, m.EnvID, m.WorkflowID, m.NotificationID, m.RunID, m.StepID, m.SubscriberID,
		m.Channel, m.ProviderID, m.Content, m.Subject, m.Status, m.Seen, m.Read, m.Archived,
		m.TransactionID, m.ErrorText, m.CreatedAt,
	)
	if err != nil {
		return domain.Message{}, err
	}
	return m, nil
}

func (repo *messageRepo) List(ctx context.Context, scope domain.Scope, subscriberID, channel string) ([]domain.Message, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, workflow_id, notification_id, run_id, step_id, subscriber_id,
		        channel, provider_id, content, subject, status, seen, read, archived, transaction_id, error_text, created_at
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
		var m domain.Message
		if err := rows.Scan(&m.ID, &m.OrgID, &m.EnvID, &m.WorkflowID, &m.NotificationID, &m.RunID, &m.StepID,
			&m.SubscriberID, &m.Channel, &m.ProviderID, &m.Content, &m.Subject, &m.Status, &m.Seen, &m.Read,
			&m.Archived, &m.TransactionID, &m.ErrorText, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
