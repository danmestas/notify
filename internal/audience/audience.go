// Package audience manages subscribers — the recipients of notifications.
// It maps external subscriber input onto domain.Subscriber and persists it
// through the Scope-first store, deduping by external subscriberId within an
// environment (mirrors the reference subscriber-job-bound upsert).
package audience

import (
	"context"
	"encoding/json"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// Service owns subscriber persistence.
type Service struct {
	store *store.Store
}

// New builds a Service over the given store.
func New(s *store.Store) *Service {
	return &Service{store: s}
}

// SubscriberInput is the external-facing profile a caller upserts. SubscriberID
// is the caller-owned external id used for dedup within a scope.
type SubscriberInput struct {
	SubscriberID string
	Email        string
	Phone        string
	FirstName    string
	LastName     string
	Locale       string
	Timezone     string
	Data         json.RawMessage
}

// Upsert maps in onto a domain.Subscriber and persists it. Re-upserting the
// same SubscriberID within scope updates the existing row in place rather than
// creating a duplicate.
func (svc *Service) Upsert(ctx context.Context, scope domain.Scope, in SubscriberInput) (domain.Subscriber, error) {
	sub := domain.Subscriber{
		SubscriberID: in.SubscriberID,
		Email:        in.Email,
		Phone:        in.Phone,
		FirstName:    in.FirstName,
		LastName:     in.LastName,
		Locale:       in.Locale,
		Timezone:     in.Timezone,
		Data:         in.Data,
	}
	return svc.store.Subscribers().Upsert(ctx, scope, sub)
}

// Get returns the subscriber identified by subscriberID within scope.
func (svc *Service) Get(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Subscriber, error) {
	return svc.store.Subscribers().GetByExternalID(ctx, scope, subscriberID)
}

// SetCredentials records per-provider channel addressing for the subscriber
// (e.g. the ntfy push topic), upserting the subscriber_channels row keyed by
// (environment, subscriber, provider). mirrors the reference
// PUT /subscribers/:id/credentials.
func (svc *Service) SetCredentials(ctx context.Context, scope domain.Scope, subscriberID, providerID, channelType string, credentials json.RawMessage) (domain.SubscriberChannel, error) {
	return svc.store.SubscriberChannels().Upsert(ctx, scope, domain.SubscriberChannel{
		SubscriberID: subscriberID,
		ProviderID:   providerID,
		Type:         channelType,
		Credentials:  credentials,
	})
}

// CreateTopic upserts a topic by (scope, key) and returns the persisted row.
func (svc *Service) CreateTopic(ctx context.Context, scope domain.Scope, key, name string) (domain.Topic, error) {
	return svc.store.Topics().Create(ctx, scope, key, name)
}

// AddTopicSubscriber records a subscriber's membership in the named topic
// within scope. An unknown topic key surfaces store.ErrNotFound.
func (svc *Service) AddTopicSubscriber(ctx context.Context, scope domain.Scope, topicKey, subscriberID string) error {
	return svc.store.Topics().AddSubscriber(ctx, scope, topicKey, subscriberID)
}
