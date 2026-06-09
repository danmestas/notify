package delivery

import (
	"context"
	"encoding/json"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// EmailResolver resolves the active email provider and the From address for a
// scope. connectivity.Service satisfies this structurally (delivery does not
// import connectivity).
type EmailResolver interface {
	ResolveEmail(ctx context.Context, scope domain.Scope) (providers.EmailProvider, string, error)
}

// EmailHandler persists rendered email messages through the store, dispatching
// via a provider resolved per scope.
type EmailHandler struct {
	store    *store.Store
	resolver EmailResolver
	prefs    PreferenceChecker
}

// NewEmail constructs an EmailHandler backed by s and resolver r.
func NewEmail(s *store.Store, r EmailResolver) *EmailHandler {
	return &EmailHandler{store: s, resolver: r}
}

// WithPreferences attaches an optional channel-preference checker and returns
// the handler for chaining. When set, a disabled channel skips the send.
func (h *EmailHandler) WithPreferences(c PreferenceChecker) *EmailHandler {
	h.prefs = c
	return h
}

// HandleEmail is registered via engine.Handle("send.email", h.HandleEmail).
// It unmarshals the run envelope, renders the subject/body templates against the
// {subscriber, payload} context, resolves the scope's email provider, sends the
// message, and persists a "sent" email message. On any unmarshal, resolve, send,
// or persist error it records an "error" message (with ErrorText) and returns
// its JSON with a nil error — a failed message, not a crashed run.
func (h *EmailHandler) HandleEmail(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var env runenv.InAppEnvelope
	if err := json.Unmarshal(t.Input, &env); err != nil {
		return h.persistEmailError(ctx, env, t, err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistEmailError(ctx, env, t, err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistEmailError(ctx, env, t, err)
		}
	}

	data := map[string]any{"subscriber": subMap, "payload": payloadMap}
	subject := template.Render(env.Step.SubjectTemplate, data)
	content := template.Render(env.Step.BodyTemplate, data)
	to, _ := subMap["email"].(string)

	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), mirroring the reference default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel)
		if perr == nil && !enabled {
			return h.persistEmailSkipped(ctx, env, t, scope, subject, content)
		}
	}
	provider, from, err := h.resolver.ResolveEmail(ctx, scope)
	if err != nil {
		return h.persistEmailError(ctx, env, t, err)
	}

	if _, err := provider.Send(ctx, providers.EmailMessage{
		From:    from,
		To:      to,
		Subject: subject,
		Text:    content,
	}); err != nil {
		return h.persistEmailError(ctx, env, t, err)
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
		Channel:        "email",
		ProviderID:     "smtp",
		Content:        content,
		Subject:        subject,
		Status:         "sent",
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	saved, err := h.store.Messages().Create(ctx, scope, msg)
	if err != nil {
		return h.persistEmailError(ctx, env, t, err)
	}
	return json.Marshal(saved)
}

// persistEmailSkipped records a status:"skipped" email message (the channel is
// disabled by subscriber preference) and returns its JSON with a nil error.
// No email is sent; this mirrors the existing skipped/failed-message pattern.
func (h *EmailHandler) persistEmailSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
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
		ProviderID:     "smtp",
		Content:        content,
		Subject:        subject,
		Status:         "skipped",
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if saved, err := h.store.Messages().Create(ctx, scope, msg); err == nil {
		return json.Marshal(saved)
	}
	return json.Marshal(msg)
}

// persistEmailError records a status:"error" email message carrying cause and
// returns its JSON with a nil error. Best-effort: if persistence itself fails,
// the in-memory message is still marshalled and returned.
func (h *EmailHandler) persistEmailError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, cause error) (json.RawMessage, error) {
	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        "email",
		ProviderID:     "smtp",
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
