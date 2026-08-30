// Package audience manages subscribers — the recipients of notifications.
// It maps external subscriber input onto domain.Subscriber and persists it
// through the Scope-first store, deduping by external subscriberId within an
// environment (mirrors the reference subscriber-job-bound upsert).
package audience

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// topicKeyPattern is the URL-safe slug allowlist for topic keys: lowercase
// letters, digits, hyphens, and underscores, starting with a letter or
// digit. A topic key is used verbatim as a console URL path segment
// (view/edit/delete/subscriber routes); a key containing '/' (path
// separator) or other reserved/URL-unsafe characters produces a topic every
// console route 404s on, permanently orphaning it (same class as H2, see
// authoring.triggerIdentifierPattern).
var topicKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ErrInvalidTopicKey is returned by CreateTopic when key fails
// topicKeyPattern. Checked before any store write.
var ErrInvalidTopicKey = fmt.Errorf("audience: topic key may only contain lowercase letters, numbers, hyphens, and underscores")

// Service owns subscriber persistence.
type Service struct {
	store *store.Store
}

// MaxSubscriberFieldLen bounds subscriber profile free-text fields (email,
// name, phone, locale, timezone) and each custom-field key/value. Enforced
// here — not just in the console handler — so a direct /v1 API call is
// bound by the same guard as the console's forms. 256 chars comfortably
// covers real-world values without letting an unbroken multi-thousand-char
// string reach storage and blow out fixed-width layouts.
const MaxSubscriberFieldLen = 256

// MaxSubscriberCustomFields caps the number of custom key/value entries a
// subscriber's Data map may hold. The console's custom-fields editor
// (subscriberCustomFieldRowPool in internal/console/subscribers.go) renders
// this many rows by default; this is the server-side enforcement so a
// direct /v1 API call can't create more fields than the console's editor is
// designed around.
//
// Subscribers that already carry more than this many fields — written
// before this guard existed, or via a direct store edit — are grandfathered
// by PatchSubscriber: an update is rejected only if it would INCREASE the
// field count past what is already stored, never merely for re-saving the
// existing data unchanged. Upsert (create / full re-upsert by external id)
// has no such row to grandfather against and enforces the flat cap.
const MaxSubscriberCustomFields = 10

// ValidationError is returned by Upsert and PatchSubscriber when the
// caller-supplied subscriber input fails validation: a malformed email, an
// oversized profile field, too many custom fields, an oversized custom-field
// key/value, or an empty custom-field key. Msg is a human-readable,
// caller-safe message that HTTP layers (console, /v1 API) can surface
// directly instead of a generic 500. Detect this case with
// errors.As(err, &target).
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

// validateSubscriberInput server-side validates the subscriber profile and
// custom-field data shared by Upsert and PatchSubscriber. maxCustomFields is
// the ceiling on len(in.Data) as a flat key/value map — callers pass
// MaxSubscriberCustomFields for a fresh subscriber, or a higher grandfathered
// value when updating a subscriber that already exceeds the cap (see
// MaxSubscriberCustomFields). Returns nil when the input is acceptable to
// persist, or a *ValidationError otherwise.
//
// Email may be empty — a subscriber can legitimately have no email address —
// but a NON-EMPTY value must be a well-formed address.
func validateSubscriberInput(in SubscriberInput, maxCustomFields int) error {
	if in.Email != "" {
		if _, err := mail.ParseAddress(in.Email); err != nil {
			return &ValidationError{Msg: "invalid email address"}
		}
	}
	fields := []struct {
		label string
		value string
	}{
		{"email", in.Email},
		{"firstName", in.FirstName},
		{"lastName", in.LastName},
		{"phone", in.Phone},
		{"locale", in.Locale},
		{"timezone", in.Timezone},
	}
	for _, f := range fields {
		if len(f.value) > MaxSubscriberFieldLen {
			return &ValidationError{Msg: fmt.Sprintf("%s is too long (max %d characters)", f.label, MaxSubscriberFieldLen)}
		}
	}
	return validateSubscriberCustomFields(in.Data, maxCustomFields)
}

// validateSubscriberCustomFields validates in.Data when it decodes as a flat
// map[string]string — the shape the console's custom-fields editor always
// produces. Richer structured JSON sent directly via the /v1 API (nested
// objects, arrays, non-string values) does not match that shape and is left
// alone here: enforcing a key/value schema on it is outside the scope of the
// custom-fields cap/length/empty-key guard.
func validateSubscriberCustomFields(data json.RawMessage, maxFields int) error {
	if len(data) == 0 {
		return nil
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	if len(fields) > maxFields {
		return &ValidationError{Msg: fmt.Sprintf("too many custom fields (max %d)", maxFields)}
	}
	for k, v := range fields {
		if strings.TrimSpace(k) == "" {
			return &ValidationError{Msg: "custom field key is required"}
		}
		if len(k) > MaxSubscriberFieldLen {
			return &ValidationError{Msg: fmt.Sprintf("custom field key is too long (max %d characters)", MaxSubscriberFieldLen)}
		}
		if len(v) > MaxSubscriberFieldLen {
			return &ValidationError{Msg: fmt.Sprintf("custom field value is too long (max %d characters)", MaxSubscriberFieldLen)}
		}
	}
	return nil
}

// countCustomFields returns the number of entries in data when it decodes as
// a flat map[string]string, or 0 when data is empty or a different shape.
// Used by PatchSubscriber to compute the grandfathered cap for a subscriber
// that already exceeds MaxSubscriberCustomFields.
func countCustomFields(data json.RawMessage) int {
	if len(data) == 0 {
		return 0
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		return 0
	}
	return len(fields)
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
	if err := validateSubscriberInput(in, MaxSubscriberCustomFields); err != nil {
		return domain.Subscriber{}, err
	}
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

// webPushSubscriptionInput is the shape of the browser PushSubscription JSON
// a caller sends as `credentials` when providerId is "webpush":
// {"endpoint":"...","keys":{"p256dh":"...","auth":"..."}}.
type webPushSubscriptionInput struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// SetWebPushCredentials registers one browser PushSubscription for the
// subscriber, upserting by endpoint into webpush_subscriptions (NOT
// subscriber_channels — a subscriber may hold many live device
// subscriptions, unlike every other provider's single credentials row).
// Called by PUT /v1/subscribers/{id}/credentials when providerId is
// "webpush"; credentials is the raw browser PushSubscription JSON.
func (svc *Service) SetWebPushCredentials(ctx context.Context, scope domain.Scope, subscriberID string, credentials json.RawMessage) (domain.WebPushSubscription, error) {
	var in webPushSubscriptionInput
	if err := json.Unmarshal(credentials, &in); err != nil {
		return domain.WebPushSubscription{}, &ValidationError{Msg: "credentials must be a PushSubscription object with endpoint and keys.p256dh/keys.auth"}
	}
	if in.Endpoint == "" || in.Keys.P256dh == "" || in.Keys.Auth == "" {
		return domain.WebPushSubscription{}, &ValidationError{Msg: "endpoint, keys.p256dh, and keys.auth are required"}
	}
	return svc.store.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: subscriberID,
		Endpoint:     in.Endpoint,
		P256dh:       in.Keys.P256dh,
		Auth:         in.Keys.Auth,
	})
}

// DeleteWebPushCredentials removes one device's Web Push subscription
// (matched by endpoint) for the subscriber within scope. Returns
// store.ErrNotFound when no such subscription exists in this scope — the
// caller (a browser unsubscribe) needs that distinct from a 204 to know
// whether anything was actually removed.
func (svc *Service) DeleteWebPushCredentials(ctx context.Context, scope domain.Scope, subscriberID, endpoint string) error {
	return svc.store.WebPushSubscriptions().DeleteByEndpoint(ctx, scope, subscriberID, endpoint)
}

// ListChannels returns the per-provider channel addressing rows recorded for
// the subscriber within scope, ordered by provider. Mirrors the read side of
// SetCredentials.
func (svc *Service) ListChannels(ctx context.Context, scope domain.Scope, subscriberID string) ([]domain.SubscriberChannel, error) {
	return svc.store.SubscriberChannels().ListBySubscriber(ctx, scope, subscriberID)
}

// CreateTopic upserts a topic by (scope, key) and returns the persisted row.
func (svc *Service) CreateTopic(ctx context.Context, scope domain.Scope, key, name string) (domain.Topic, error) {
	if !topicKeyPattern.MatchString(key) {
		return domain.Topic{}, ErrInvalidTopicKey
	}
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
// within scope. A soft-deleted subscriber is treated as not found: callers
// (e.g. the console detail page) must not render deleted records.
func (svc *Service) GetSubscriberByID(ctx context.Context, scope domain.Scope, id string) (domain.Subscriber, error) {
	sub, err := svc.store.Subscribers().GetByID(ctx, scope, id)
	if err != nil {
		return domain.Subscriber{}, err
	}
	if sub.Deleted {
		return domain.Subscriber{}, store.ErrNotFound
	}
	return sub, nil
}

// PatchSubscriber updates mutable profile fields on the subscriber identified by
// its internal ULID id within scope. Only non-empty fields in in are applied.
func (svc *Service) PatchSubscriber(ctx context.Context, scope domain.Scope, id string, in SubscriberInput) (domain.Subscriber, error) {
	existing, err := svc.store.Subscribers().GetByID(ctx, scope, id)
	if err != nil {
		return domain.Subscriber{}, err
	}

	maxCustomFields := MaxSubscriberCustomFields
	if baseline := countCustomFields(existing.Data); baseline > maxCustomFields {
		// Grandfather: this subscriber already carries more custom fields
		// than the cap allows. Only reject if the update would grow the
		// count further — re-saving the existing data unchanged (e.g. the
		// console edit form's Save button) must never be blocked by the cap.
		maxCustomFields = baseline
	}
	if err := validateSubscriberInput(in, maxCustomFields); err != nil {
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

// PurgeDeleted hard-deletes ONLY the soft-deleted row for the given external
// subscriberID within scope. The underlying store call is scoped to deleted=1
// rows so a concurrently-inserted live row is never removed.
func (svc *Service) PurgeDeleted(ctx context.Context, scope domain.Scope, subscriberID string) (int64, error) {
	return svc.store.Subscribers().PurgeDeletedByExternalID(ctx, scope, subscriberID)
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
