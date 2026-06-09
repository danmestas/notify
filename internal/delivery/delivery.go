// Package delivery implements channel send handlers registered with the engine.
// HandleInApp is the send.in_app task handler: it renders the authored subject
// and body templates against the run envelope and persists the resulting in-app
// message. mirrors the reference behaviour of recording a failed message (status
// "error") rather than crashing the run when the envelope is unusable.
package delivery

import (
	"context"
	"encoding/json"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// PreferenceChecker reports whether a channel is enabled for a subscriber/workflow.
// preferences.Service satisfies this structurally (delivery does not import
// preferences). A nil checker means "always send" (no preference filtering).
type PreferenceChecker interface {
	ChannelEnabled(ctx context.Context, scope domain.Scope, subscriberID, workflowID, channel string) (bool, error)
}

// EventPublisher publishes live events for a persisted message: a per-subscriber
// inbox event and an environment-wide delivery-status event. eventbus.Bus
// satisfies this structurally (delivery does not import eventbus). A nil
// publisher means "no live publish".
type EventPublisher interface {
	PublishInbox(environmentID, subscriberID string, data []byte) error
	PublishDelivery(environmentID string, data []byte) error
}

// Handler persists rendered messages through the store.
type Handler struct {
	store *store.Store
	prefs PreferenceChecker
	pub   EventPublisher
}

// New constructs a Handler backed by s.
func New(s *store.Store) *Handler {
	return &Handler{store: s}
}

// WithPreferences attaches an optional channel-preference checker and returns
// the handler for chaining. When set, a disabled channel skips the send.
func (h *Handler) WithPreferences(c PreferenceChecker) *Handler {
	h.prefs = c
	return h
}

// WithEventBus attaches an optional live event publisher and returns the
// handler for chaining. When set, a sent in-app message is published to the
// inbox best-effort and every terminal message is published as a
// delivery-status event; a nil publisher disables live publishing.
func (h *Handler) WithEventBus(p EventPublisher) *Handler {
	h.pub = p
	return h
}

// HandleInApp is registered via engine.Handle("send.in_app", h.HandleInApp).
// It unmarshals the run envelope, renders the subject/body templates against the
// {subscriber, payload} context, and persists a "sent" in-app message. On any
// unmarshal or persist error it records a "error" message (with ErrorText) and
// returns its JSON with a nil error — a failed message, not a crashed run.
func (h *Handler) HandleInApp(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var env runenv.InAppEnvelope
	if err := json.Unmarshal(t.Input, &env); err != nil {
		// Envelope undecodable: scope/subscriber are unknown (empty); still
		// record a failed message so the failure is observable.
		return h.persistError(ctx, env, t, err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistError(ctx, env, t, err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistError(ctx, env, t, err)
		}
	}

	data := map[string]any{"subscriber": subMap, "payload": payloadMap}
	subject := template.Render(env.Step.SubjectTemplate, data)
	content := template.Render(env.Step.BodyTemplate, data)

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), mirroring the reference default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, env.SubscriberID, env.WorkflowID, env.Step.Channel)
		if perr == nil && !enabled {
			return h.persistSkipped(ctx, env, t, subject, content)
		}
	}

	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        "in_app",
		ProviderID:     env.Step.ProviderID,
		Content:        content,
		Subject:        subject,
		Status:         "sent",
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	saved, err := h.store.Messages().Create(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, msg)
	if err != nil {
		return h.persistError(ctx, env, t, err)
	}
	out, err := json.Marshal(saved)
	if err != nil {
		return out, err
	}
	// Best-effort live publishes for the sent message. A publish failure must
	// not fail the task — the message is already persisted. The inbox event is
	// per-subscriber; the delivery-status event drives outbound webhooks.
	if h.pub != nil {
		_ = h.pub.PublishInbox(env.Scope.EnvID, env.SubscriberID, out)
		_ = h.pub.PublishDelivery(env.Scope.EnvID, out)
	}
	return out, nil
}

// persistSkipped records a status:"skipped" in-app message (the channel is
// disabled by subscriber preference) and returns its JSON with a nil error.
// No send occurs; this mirrors the existing skipped/failed-message pattern.
func (h *Handler) persistSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, subject, content string) (json.RawMessage, error) {
	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        env.Step.Channel,
		ProviderID:     env.Step.ProviderID,
		Content:        content,
		Subject:        subject,
		Status:         "skipped",
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	saved, err := h.store.Messages().Create(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, msg)
	if err != nil {
		saved = msg
	}
	out, merr := json.Marshal(saved)
	h.publishDelivery(env.Scope.EnvID, out)
	return out, merr
}

// persistError records a status:"error" message carrying cause and returns its
// JSON with a nil error. Best-effort: if persistence itself fails, the in-memory
// message is still marshalled and returned so the caller observes the failure.
func (h *Handler) persistError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, cause error) (json.RawMessage, error) {
	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        "in_app",
		ProviderID:     env.Step.ProviderID,
		Status:         "error",
		ErrorText:      cause.Error(),
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	saved, err := h.store.Messages().Create(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, msg)
	if err != nil {
		saved = msg
	}
	out, merr := json.Marshal(saved)
	h.publishDelivery(env.Scope.EnvID, out)
	return out, merr
}

// publishDelivery emits a best-effort delivery-status event carrying the
// message JSON. A nil publisher or a publish failure is ignored — it must
// never fail the task.
func (h *Handler) publishDelivery(envID string, data []byte) {
	if h.pub != nil {
		_ = h.pub.PublishDelivery(envID, data)
	}
}
