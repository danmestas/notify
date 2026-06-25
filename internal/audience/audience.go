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

// ListChannels returns the per-provider channel addressing rows recorded for
// the subscriber within scope, ordered by provider. Mirrors the read side of
// SetCredentials.
func (svc *Service) ListChannels(ctx context.Context, scope domain.Scope, subscriberID string) ([]domain.SubscriberChannel, error) {
	return svc.store.SubscriberChannels().ListBySubscriber(ctx, scope, subscriberID)
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

// ListSubscribers returns a cursor-paginated page of non-deleted subscribers
// within scope. after="" starts from the first page. Calls through to
// store.SubscriberRepo.List.
func (svc *Service) ListSubscribers(ctx context.Context, scope domain.Scope, after string, limit int) (store.SubscriberPage, error) {
	return svc.store.Subscribers().List(ctx, scope, after, limit)
}

// GetSubscriberByID returns the subscriber identified by its internal ULID id
// within scope.
func (svc *Service) GetSubscriberByID(ctx context.Context, scope domain.Scope, id string) (domain.Subscriber, error) {
	return svc.store.Subscribers().GetByID(ctx, scope, id)
}

// PatchSubscriber updates mutable profile fields on the subscriber identified by
// its internal ULID id within scope. Only non-empty fields in in are applied.
func (svc *Service) PatchSubscriber(ctx context.Context, scope domain.Scope, id string, in SubscriberInput) (domain.Subscriber, error) {
	existing, err := svc.store.Subscribers().GetByID(ctx, scope, id)
	if err != nil {
		return domain.Subscriber{}, err
	}
	// Merge: non-empty incoming fields overwrite the stored value.
	if in.Email != "" {
		existing.Email = in.Email
	}
	if in.FirstName != "" {
		existing.FirstName = in.FirstName
	}
	if in.LastName != "" {
		existing.LastName = in.LastName
	}
	if in.Phone != "" {
		existing.Phone = in.Phone
	}
	if in.Locale != "" {
		existing.Locale = in.Locale
	}
	if in.Timezone != "" {
		existing.Timezone = in.Timezone
	}
	if len(in.Data) > 0 {
		existing.Data = in.Data
	}
	return svc.store.Subscribers().Upsert(ctx, scope, existing)
}

// DeleteSubscriber soft-deletes the subscriber identified by its internal ULID
// id within scope.
func (svc *Service) DeleteSubscriber(ctx context.Context, scope domain.Scope, id string) error {
	return svc.store.Subscribers().SoftDelete(ctx, scope, id)
}

// ListTopics returns all topics in scope annotated with their member counts.
func (svc *Service) ListTopics(ctx context.Context, scope domain.Scope) ([]store.TopicWithCount, error) {
	return svc.store.Topics().List(ctx, scope)
}

// GetTopic returns the topic identified by key within scope, annotated with
// its current member count (single-topic reads agree with ListTopics).
func (svc *Service) GetTopic(ctx context.Context, scope domain.Scope, key string) (store.TopicWithCount, error) {
	return svc.store.Topics().GetByKey(ctx, scope, key)
}

// DeleteTopic removes a topic and all its memberships permanently. Returns
// store.ErrNotFound when the topic does not exist.
func (svc *Service) DeleteTopic(ctx context.Context, scope domain.Scope, key string) error {
	return svc.store.Topics().Delete(ctx, scope, key)
}

// RemoveTopicSubscriber removes a single subscriber's membership from a topic.
func (svc *Service) RemoveTopicSubscriber(ctx context.Context, scope domain.Scope, topicKey, subscriberID string) error {
	return svc.store.Topics().RemoveSubscriber(ctx, scope, topicKey, subscriberID)
}

// ListTopicSubscriberIDs returns the external subscriber IDs (caller-owned
// subscriber_id values) that are members of the named topic within scope.
func (svc *Service) ListTopicSubscriberIDs(ctx context.Context, scope domain.Scope, topicKey string) ([]string, error) {
	return svc.store.Topics().ListSubscriberIDs(ctx, scope, topicKey)
}
