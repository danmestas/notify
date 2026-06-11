package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// SubscriberChannelRepo persists per-subscriber channel addressing (push
// topics, chat webhooks). Every method is Scope-first; the dedup key is
// (environment_id, subscriber_id, provider_id).
type SubscriberChannelRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, ch domain.SubscriberChannel) (domain.SubscriberChannel, error)
	GetByProvider(ctx context.Context, scope domain.Scope, subscriberID, providerID string) (domain.SubscriberChannel, error)
}

type subscriberChannelRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *subscriberChannelRepo) Upsert(ctx context.Context, scope domain.Scope, ch domain.SubscriberChannel) (domain.SubscriberChannel, error) {
	ch.OrgID = scope.OrgID
	ch.EnvID = scope.EnvID
	if ch.ID == "" {
		ch.ID = newULID()
	}
	// On conflict of the (environment_id, subscriber_id, provider_id) dedup
	// key, replace the channel type and credentials in place: re-registering a
	// device/topic updates the existing row. id stays the original row's id.
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO subscriber_channels
		   (id, org_id, environment_id, subscriber_id, provider_id, type, credentials)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, subscriber_id, provider_id)
		 DO UPDATE SET type = excluded.type, credentials = excluded.credentials`,
		ch.ID, ch.OrgID, ch.EnvID, ch.SubscriberID, ch.ProviderID, ch.Type, []byte(ch.Credentials),
	)
	if err != nil {
		return domain.SubscriberChannel{}, err
	}
	return repo.GetByProvider(ctx, scope, ch.SubscriberID, ch.ProviderID)
}

func (repo *subscriberChannelRepo) GetByProvider(ctx context.Context, scope domain.Scope, subscriberID, providerID string) (domain.SubscriberChannel, error) {
	var (
		ch    domain.SubscriberChannel
		creds []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, provider_id, type, credentials
		 FROM subscriber_channels
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND provider_id = ?`,
		scope.OrgID, scope.EnvID, subscriberID, providerID,
	).Scan(&ch.ID, &ch.OrgID, &ch.EnvID, &ch.SubscriberID, &ch.ProviderID, &ch.Type, &creds)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SubscriberChannel{}, ErrNotFound
	}
	if err != nil {
		return domain.SubscriberChannel{}, err
	}
	ch.Credentials = creds
	return ch, nil
}
