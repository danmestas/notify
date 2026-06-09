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

// Handler persists rendered messages through the store.
type Handler struct {
	store *store.Store
}

// New constructs a Handler backed by s.
func New(s *store.Store) *Handler {
	return &Handler{store: s}
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
	return json.Marshal(saved)
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
	if saved, err := h.store.Messages().Create(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, msg); err == nil {
		return json.Marshal(saved)
	}
	return json.Marshal(msg)
}
