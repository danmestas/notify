package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// SubscriberRepo persists subscribers keyed by (EnvID, SubscriberID). Every
// method is Scope-first; the unique dedup key is (environment_id, subscriber_id).
type SubscriberRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, sub domain.Subscriber) (domain.Subscriber, error)
	GetByExternalID(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Subscriber, error)
	List(ctx context.Context, scope domain.Scope) ([]domain.Subscriber, error)
}

type subscriberRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *subscriberRepo) Upsert(ctx context.Context, scope domain.Scope, sub domain.Subscriber) (domain.Subscriber, error) {
	sub.OrgID = scope.OrgID
	sub.EnvID = scope.EnvID
	if sub.ID == "" {
		sub.ID = newULID()
	}
	// On conflict of the (environment_id, subscriber_id) dedup key, MERGE the
	// mutable profile in place: each column takes the incoming value only when
	// it is non-empty, otherwise it keeps the stored value. This mirrors the reference
	// subscriber patch-upsert — a partial re-upsert (e.g. a trigger carrying
	// only `to.subscriberId`) must not blank out a profile populated by an
	// earlier full upsert. id stays the original row's id.
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO subscribers
		   (id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, subscriber_id) DO UPDATE SET
		   email=COALESCE(NULLIF(excluded.email, ''), subscribers.email),
		   phone=COALESCE(NULLIF(excluded.phone, ''), subscribers.phone),
		   first_name=COALESCE(NULLIF(excluded.first_name, ''), subscribers.first_name),
		   last_name=COALESCE(NULLIF(excluded.last_name, ''), subscribers.last_name),
		   locale=COALESCE(NULLIF(excluded.locale, ''), subscribers.locale),
		   timezone=COALESCE(NULLIF(excluded.timezone, ''), subscribers.timezone),
		   data=COALESCE(NULLIF(excluded.data, X''), subscribers.data),
		   deleted=excluded.deleted`,
		sub.ID, sub.OrgID, sub.EnvID, sub.SubscriberID, sub.Email, sub.Phone,
		sub.FirstName, sub.LastName, sub.Locale, sub.Timezone, []byte(sub.Data), sub.Deleted,
	)
	if err != nil {
		return domain.Subscriber{}, err
	}
	return repo.GetByExternalID(ctx, scope, sub.SubscriberID)
}

func (repo *subscriberRepo) GetByExternalID(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Subscriber, error) {
	var (
		sub  domain.Subscriber
		data []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted
		 FROM subscribers WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?`,
		scope.OrgID, scope.EnvID, subscriberID,
	).Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Email, &sub.Phone,
		&sub.FirstName, &sub.LastName, &sub.Locale, &sub.Timezone, &data, &sub.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Subscriber{}, ErrNotFound
	}
	if err != nil {
		return domain.Subscriber{}, err
	}
	sub.Data = data
	return sub, nil
}

func (repo *subscriberRepo) List(ctx context.Context, scope domain.Scope) ([]domain.Subscriber, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted
		 FROM subscribers WHERE org_id = ? AND environment_id = ? ORDER BY id`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscriber
	for rows.Next() {
		var (
			sub  domain.Subscriber
			data []byte
		)
		if err := rows.Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Email, &sub.Phone,
			&sub.FirstName, &sub.LastName, &sub.Locale, &sub.Timezone, &data, &sub.Deleted); err != nil {
			return nil, err
		}
		sub.Data = data
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}
