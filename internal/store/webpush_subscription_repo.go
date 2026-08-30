package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// WebPushSubscriptionRepo persists per-device Web Push subscriptions. Every
// method is Scope-first; the dedup key is (environment_id, subscriber_id,
// endpoint) — a subscriber may hold many rows (one per device), unlike
// SubscriberChannelRepo's one-row-per-provider contract.
type WebPushSubscriptionRepo interface {
	// UpsertByEndpoint inserts a new subscription or, when the
	// (environment, subscriber, endpoint) key already exists, refreshes its
	// keys and last_used_at in place. created_at is stamped once, at first
	// insert, and left unchanged on update.
	UpsertByEndpoint(ctx context.Context, scope domain.Scope, sub domain.WebPushSubscription) (domain.WebPushSubscription, error)
	// ListBySubscriber returns every device subscription registered for the
	// subscriber within scope, ordered by created_at then id.
	ListBySubscriber(ctx context.Context, scope domain.Scope, subscriberID string) ([]domain.WebPushSubscription, error)
	// DeleteByEndpoint removes one device subscription. Returns ErrNotFound
	// when the row does not exist in this scope.
	DeleteByEndpoint(ctx context.Context, scope domain.Scope, subscriberID, endpoint string) error
}

type webPushSubscriptionRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *webPushSubscriptionRepo) UpsertByEndpoint(ctx context.Context, scope domain.Scope, sub domain.WebPushSubscription) (domain.WebPushSubscription, error) {
	sub.OrgID = scope.OrgID
	sub.EnvID = scope.EnvID
	if sub.ID == "" {
		sub.ID = newULID()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO webpush_subscriptions
		   (id, org_id, environment_id, subscriber_id, endpoint, p256dh, auth, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, subscriber_id, endpoint)
		 DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, last_used_at = excluded.last_used_at`,
		sub.ID, sub.OrgID, sub.EnvID, sub.SubscriberID, sub.Endpoint, sub.P256dh, sub.Auth, now, now,
	)
	if err != nil {
		return domain.WebPushSubscription{}, err
	}
	return repo.getByEndpoint(ctx, scope, sub.SubscriberID, sub.Endpoint)
}

func (repo *webPushSubscriptionRepo) getByEndpoint(ctx context.Context, scope domain.Scope, subscriberID, endpoint string) (domain.WebPushSubscription, error) {
	var sub domain.WebPushSubscription
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, endpoint, p256dh, auth, created_at, last_used_at
		 FROM webpush_subscriptions
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND endpoint = ?`,
		scope.OrgID, scope.EnvID, subscriberID, endpoint,
	).Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.CreatedAt, &sub.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebPushSubscription{}, ErrNotFound
	}
	if err != nil {
		return domain.WebPushSubscription{}, err
	}
	return sub, nil
}

func (repo *webPushSubscriptionRepo) ListBySubscriber(ctx context.Context, scope domain.Scope, subscriberID string) ([]domain.WebPushSubscription, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, endpoint, p256dh, auth, created_at, last_used_at
		 FROM webpush_subscriptions
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?
		 ORDER BY created_at ASC, id ASC`,
		scope.OrgID, scope.EnvID, subscriberID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.WebPushSubscription
	for rows.Next() {
		var sub domain.WebPushSubscription
		if err := rows.Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.CreatedAt, &sub.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (repo *webPushSubscriptionRepo) DeleteByEndpoint(ctx context.Context, scope domain.Scope, subscriberID, endpoint string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM webpush_subscriptions
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND endpoint = ?`,
		scope.OrgID, scope.EnvID, subscriberID, endpoint,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
