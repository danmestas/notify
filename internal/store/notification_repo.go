package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// NotificationRepo persists notification records (one per trigger fan-out).
// Scope-first throughout.
type NotificationRepo interface {
	Create(ctx context.Context, scope domain.Scope, n domain.Notification) (domain.Notification, error)
	Get(ctx context.Context, scope domain.Scope, id string) (domain.Notification, error)
	GetByRunID(ctx context.Context, runID string) (domain.Notification, error)
}

type notificationRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *notificationRepo) Create(ctx context.Context, scope domain.Scope, n domain.Notification) (domain.Notification, error) {
	n.OrgID = scope.OrgID
	n.EnvID = scope.EnvID
	if n.ID == "" {
		n.ID = newULID()
	}
	if n.CreatedAt == "" {
		n.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO notifications
		   (id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.OrgID, n.EnvID, n.WorkflowID, n.SubscriberID, n.TransactionID, n.RunID,
		[]byte(n.Payload), n.CreatedAt, n.DigestKey,
	)
	if err != nil {
		return domain.Notification{}, err
	}
	return n, nil
}

func (repo *notificationRepo) Get(ctx context.Context, scope domain.Scope, id string) (domain.Notification, error) {
	var (
		n       domain.Notification
		payload []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key
		 FROM notifications WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&n.ID, &n.OrgID, &n.EnvID, &n.WorkflowID, &n.SubscriberID, &n.TransactionID, &n.RunID,
		&payload, &n.CreatedAt, &n.DigestKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, err
	}
	n.Payload = payload
	return n, nil
}

// GetByRunID resolves a notification by its globally-unique dagnats run id.
// Unscoped by design: a worker handling a task has only the opaque RunID (no
// tenancy ctx); the returned row carries OrgID/EnvID for subsequent scoped use.
func (repo *notificationRepo) GetByRunID(ctx context.Context, runID string) (domain.Notification, error) {
	var (
		n       domain.Notification
		payload []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key
		 FROM notifications WHERE run_id = ? ORDER BY created_at DESC LIMIT 1`,
		runID,
	).Scan(&n.ID, &n.OrgID, &n.EnvID, &n.WorkflowID, &n.SubscriberID, &n.TransactionID, &n.RunID,
		&payload, &n.CreatedAt, &n.DigestKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, err
	}
	n.Payload = payload
	return n, nil
}
