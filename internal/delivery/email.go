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

// EmailResolver resolves the active email provider, the From address, the
// provider ID, and the integration ID for a scope. connectivity.Service
// satisfies this structurally (delivery does not import connectivity).
//
// Return order: (provider, from, providerID, integrationID, error).
// providerID is the actual provider string from the integration row, fixing
// the hardcoded "smtp" bug (issue #20).
// integrationID is the row's primary key, used to populate
// domain.Message.IntegrationID for Activity tab filtering (BP-13b).
type EmailResolver interface {
	ResolveEmail(ctx context.Context, scope domain.Scope) (providers.EmailProvider, string, string, string, error)
}

// EmailHandler persists rendered email messages through the store, dispatching
// via a provider resolved per scope.
type EmailHandler struct {
	store    *store.Store
	resolver EmailResolver
	prefs    PreferenceChecker
	pub      EventPublisher
	throttle Throttler
	digester Digester
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

// WithEventBus attaches an optional live event publisher and returns the
// handler for chaining. When set, every terminal email message is published
// as a delivery-status event best-effort; a nil publisher disables it.
func (h *EmailHandler) WithEventBus(p EventPublisher) *EmailHandler {
	h.pub = p
	return h
}

// WithThrottle attaches an optional per-(env,subscriber,workflow) rate limiter
// and returns the handler for chaining. When set, a step that configures a
// throttle limit records a "throttled" message instead of sending once the
// limit is exceeded; a nil throttler disables throttling.
func (h *EmailHandler) WithThrottle(t Throttler) *EmailHandler {
	h.throttle = t
	return h
}

// WithDigester attaches an optional digester and returns the handler for
// chaining. When set, a self-sourced notification carrying a DigestKey drains
// its merged events and exposes {{digest.count}} to the templates; a nil
// digester or a non-digest notification renders exactly as before.
func (h *EmailHandler) WithDigester(d Digester) *EmailHandler {
	h.digester = d
	return h
}

// publishDelivery emits a best-effort delivery-status event carrying the
// message JSON. A nil publisher or a publish failure is ignored — it must
// never fail the task.
func (h *EmailHandler) publishDelivery(envID string, data []byte) {
	if h.pub != nil {
		_ = h.pub.PublishDelivery(envID, data)
	}
}

// resolveEmailIDs calls h.resolver.ResolveEmail and returns the providerID and
// integrationID strings; ignores the provider and from returns. Used on
// pre-send paths (skipped, throttled) where we want the IDs for attribution
// but the provider itself is not needed. A resolve failure returns empty
// strings; callers treat "" as "unknown" and continue rather than blocking.
func (h *EmailHandler) resolveEmailIDs(ctx context.Context, scope domain.Scope) (providerID, integrationID string) {
	_, _, pid, iid, err := h.resolver.ResolveEmail(ctx, scope)
	if err != nil {
		return "", ""
	}
	return pid, iid
}

// HandleEmail is registered via engine.Handle("send.email", h.HandleEmail).
// It unmarshals the run envelope, renders the subject/body templates against the
// {subscriber, payload} context, resolves the scope's email provider, sends the
// message, and persists a "sent" email message. On any unmarshal, resolve, send,
// or persist error it records an "error" message (with ErrorText) and returns
// its JSON with a nil error — a failed message, not a crashed run.
func (h *EmailHandler) HandleEmail(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var env runenv.InAppEnvelope
	var digestKey string
	if len(t.Input) == 0 {
		// Self-source: an empty input means an upstream step (e.g. a delay)
		// produced no output, so rebuild the envelope from the store by run id.
		rebuilt, dk, err := rebuildEnvelope(ctx, h.store, t)
		if err != nil {
			return h.persistEmailError(ctx, env, t, "", "", err)
		}
		env = rebuilt
		digestKey = dk
	} else if err := json.Unmarshal(t.Input, &env); err != nil {
		return h.persistEmailError(ctx, env, t, "", "", err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistEmailError(ctx, env, t, "", "", err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistEmailError(ctx, env, t, "", "", err)
		}
	}

	data := map[string]any{"subscriber": subMap, "payload": payloadMap}
	// Digest aggregate: a self-sourced notification with a DigestKey drains the
	// merged events so the body can render {{digest.count}}. Only the
	// self-source path sets digestKey, so the Input-present path is unchanged.
	if digestKey != "" && h.digester != nil {
		events, _ := h.digester.DigestDrain(digestKey)
		data["digest"] = map[string]any{"count": len(events)}
	}
	translate := storeTranslator(ctx, h.store, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, subMap)
	subject := template.RenderI18n(env.Step.SubjectTemplate, data, translate)
	content := template.RenderI18n(env.Step.BodyTemplate, data, translate)
	to, _ := subMap["email"].(string)

	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
	content = wrapLayout(ctx, h.store, scope, env.Step.LayoutKey, content, data, translate)

	// Per-step condition gate. When the step authored a filter condition, it is
	// evaluated against the {subscriber, payload} context; a false (or
	// unevaluable) condition SKIPS the send. Absent/empty filters always run. A
	// malformed condition fails closed (skip) so a broken condition never blasts
	// a send to every subscriber.
	if !conditionMet(env.Step.Filters, subMap, payloadMap) {
		return h.persistEmailSkipped(ctx, env, t, scope, subject, content)
	}

	// Optional throttle gate. When the step configures a limit and a throttler
	// is attached, a send over the per-(env,subscriber,workflow) limit records a
	// "throttled" message instead of sending.
	if env.Step.ThrottleLimit > 0 && h.throttle != nil {
		key := env.Scope.EnvID + ":" + env.SubscriberID + ":" + env.WorkflowID
		allowed, _ := h.throttle.ThrottleAllow(key, env.Step.ThrottleLimit, time.Duration(env.Step.ThrottleWindowSec)*time.Second)
		if !allowed {
			return h.persistEmailThrottled(ctx, env, t, scope, subject, content)
		}
	}

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), mirroring the reference default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel)
		if perr == nil && !enabled {
			return h.persistEmailSkipped(ctx, env, t, scope, subject, content)
		}
	}
	provider, from, providerID, integrationID, err := h.resolver.ResolveEmail(ctx, scope)
	if err != nil {
		return h.persistEmailError(ctx, env, t, "", "", err)
	}

	if _, err := provider.Send(ctx, providers.EmailMessage{
		From:    from,
		To:      to,
		Subject: subject,
		Text:    content,
	}); err != nil {
		return h.persistEmailError(ctx, env, t, providerID, integrationID, err)
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
		ProviderID:     providerID,
		IntegrationID:  integrationID,
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
		return h.persistEmailError(ctx, env, t, providerID, integrationID, err)
	}
	out, merr := json.Marshal(saved)
	if merr != nil {
		return out, merr
	}
	h.publishDelivery(env.Scope.EnvID, out)
	return out, nil
}

// persistEmailSkipped records a status:"skipped" email message (the channel is
// disabled by subscriber preference) and returns its JSON with a nil error.
// No email is sent; this mirrors the existing skipped/failed-message pattern.
// resolve is attempted best-effort to populate providerID and integrationID.
func (h *EmailHandler) persistEmailSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	providerID, integrationID := h.resolveEmailIDs(ctx, scope)
	if providerID == "" {
		providerID = "smtp" // safe default; resolve failure must not block the record
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
		Channel:        env.Step.Channel,
		ProviderID:     providerID,
		IntegrationID:  integrationID,
		Content:        content,
		Subject:        subject,
		Status:         "skipped",
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	saved, err := h.store.Messages().Create(ctx, scope, msg)
	if err != nil {
		saved = msg
	}
	out, merr := json.Marshal(saved)
	h.publishDelivery(env.Scope.EnvID, out)
	return out, merr
}

// persistEmailThrottled records a status:"throttled" email message (the send
// exceeded the step's per-(env,subscriber,workflow) rate limit) and returns its
// JSON with a nil error. No email is sent; mirrors the persistEmailSkipped pattern.
func (h *EmailHandler) persistEmailThrottled(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	providerID, integrationID := h.resolveEmailIDs(ctx, scope)
	if providerID == "" {
		providerID = "smtp"
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
		Channel:        env.Step.Channel,
		ProviderID:     providerID,
		IntegrationID:  integrationID,
		Content:        content,
		Subject:        subject,
		Status:         "throttled",
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	saved, err := h.store.Messages().Create(ctx, scope, msg)
	if err != nil {
		saved = msg
	}
	out, merr := json.Marshal(saved)
	h.publishDelivery(env.Scope.EnvID, out)
	return out, merr
}

// persistEmailError records a status:"error" email message carrying cause and
// returns its JSON with a nil error. Best-effort: if persistence itself fails,
// the in-memory message is still marshalled and returned.
// providerID and integrationID are empty on pre-resolve paths and carry the
// actual values on post-resolve paths (send failure, persist failure).
func (h *EmailHandler) persistEmailError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, providerID, integrationID string, cause error) (json.RawMessage, error) {
	if providerID == "" {
		providerID = "smtp" // safe default for pre-resolve error rows
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
		ProviderID:     providerID,
		IntegrationID:  integrationID,
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
