package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// PushResolver resolves the active push provider, provider ID, and integration
// ID for a scope. connectivity.Service satisfies this structurally (delivery
// does not import connectivity).
//
// Return order: (provider, providerID, integrationID, error).
// integrationID is the row's primary key, used to populate
// domain.Message.IntegrationID for Activity tab filtering (BP-13b).
type PushResolver interface {
	ResolvePush(ctx context.Context, scope domain.Scope) (providers.PushProvider, string, string, error)
}

// PushHandler persists rendered push messages through the store, dispatching
// via a provider resolved per scope and a topic resolved per subscriber
// (subscriber_channels credentials JSON, e.g. {"topic":"agent-alerts"}).
type PushHandler struct {
	store    *store.Store
	resolver PushResolver
	prefs    PreferenceChecker
	pub      EventPublisher
	throttle Throttler
	digester Digester
}

// NewPush constructs a PushHandler backed by s and resolver r.
func NewPush(s *store.Store, r PushResolver) *PushHandler {
	return &PushHandler{store: s, resolver: r}
}

// WithPreferences attaches an optional channel-preference checker and returns
// the handler for chaining. When set, a disabled channel skips the send.
func (h *PushHandler) WithPreferences(c PreferenceChecker) *PushHandler {
	h.prefs = c
	return h
}

// WithEventBus attaches an optional live event publisher and returns the
// handler for chaining. When set, every terminal push message is published
// as a delivery-status event best-effort; a nil publisher disables it.
func (h *PushHandler) WithEventBus(p EventPublisher) *PushHandler {
	h.pub = p
	return h
}

// WithThrottle attaches an optional per-(env,subscriber,workflow) rate limiter
// and returns the handler for chaining. When set, a step that configures a
// throttle limit records a "throttled" message instead of sending once the
// limit is exceeded; a nil throttler disables throttling.
func (h *PushHandler) WithThrottle(t Throttler) *PushHandler {
	h.throttle = t
	return h
}

// WithDigester attaches an optional digester and returns the handler for
// chaining. When set, a self-sourced notification carrying a DigestKey drains
// its merged events and exposes {{digest.count}} to the templates; a nil
// digester or a non-digest notification renders exactly as before.
func (h *PushHandler) WithDigester(d Digester) *PushHandler {
	h.digester = d
	return h
}

// publishDelivery emits a best-effort delivery-status event carrying the
// message JSON. A nil publisher or a publish failure is ignored — it must
// never fail the task.
func (h *PushHandler) publishDelivery(envID string, data []byte) {
	if h.pub != nil {
		_ = h.pub.PublishDelivery(envID, data)
	}
}

// resolvePushIDs calls h.resolver.ResolvePush and returns the providerID and
// integrationID strings only. Used on pre-send paths (skipped, throttled) where
// we want the IDs for attribution but the provider itself is not needed. A
// resolve failure returns empty strings; callers treat "" as "unknown" and
// continue rather than blocking the persist.
func (h *PushHandler) resolvePushIDs(ctx context.Context, scope domain.Scope) (providerID, integrationID string) {
	_, pid, iid, err := h.resolver.ResolvePush(ctx, scope)
	if err != nil {
		return "", ""
	}
	return pid, iid
}

// HandlePush is registered via engine.Handle("send.push", h.HandlePush).
// It unmarshals the run envelope, renders the title/body templates against the
// {subscriber, payload} context, resolves the scope's push provider and the
// subscriber's topic, sends the message, and persists a "sent" push message.
// Failures are classified per 0001 §10: a transient provider fault (5xx,
// timeout) returns engine.NewRetryableError so the engine retries the task;
// every permanent fault (unmarshal, resolve, missing topic, 4xx, persist)
// records an "error" message (with ErrorText) and returns its JSON with a nil
// error — a failed message, not a crashed run.
func (h *PushHandler) HandlePush(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var env runenv.InAppEnvelope
	var digestKey string
	if len(t.Input) == 0 {
		// Self-source: an empty input means an upstream step (e.g. a delay)
		// produced no output, so rebuild the envelope from the store by run id.
		rebuilt, dk, err := rebuildEnvelope(ctx, h.store, t)
		if err != nil {
			return h.persistPushError(ctx, env, t, "ntfy", "", err)
		}
		env = rebuilt
		digestKey = dk
	} else if err := json.Unmarshal(t.Input, &env); err != nil {
		return h.persistPushError(ctx, env, t, "ntfy", "", err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistPushError(ctx, env, t, "ntfy", "", err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistPushError(ctx, env, t, "ntfy", "", err)
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
	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
	translate := storeTranslator(ctx, h.store, scope, subMap)
	subject := template.RenderI18n(env.Step.SubjectTemplate, data, translate)
	content := template.RenderI18n(env.Step.BodyTemplate, data, translate)

	// Optional throttle gate. When the step configures a limit and a throttler
	// is attached, a send over the per-(env,subscriber,workflow) limit records a
	// "throttled" message instead of sending.
	if env.Step.ThrottleLimit > 0 && h.throttle != nil {
		key := env.Scope.EnvID + ":" + env.SubscriberID + ":" + env.WorkflowID
		allowed, _ := h.throttle.ThrottleAllow(key, env.Step.ThrottleLimit, time.Duration(env.Step.ThrottleWindowSec)*time.Second)
		if !allowed {
			return h.persistPushThrottled(ctx, env, t, scope, subject, content)
		}
	}

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), mirroring the reference default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel)
		if perr == nil && !enabled {
			return h.persistPushSkipped(ctx, env, t, scope, subject, content)
		}
	}

	provider, providerID, integrationID, err := h.resolver.ResolvePush(ctx, scope)
	if err != nil {
		return h.persistPushError(ctx, env, t, "ntfy", "", err)
	}

	// Per-subscriber addressing: the topic comes from the subscriber_channels
	// row for this provider; an unaddressable subscriber is a permanent fault.
	// From here on error rows carry the resolved provider id, so they cannot
	// disagree with sent rows when another push provider is active.
	ch, err := h.store.SubscriberChannels().GetByProvider(ctx, scope, env.SubscriberID, providerID)
	if err != nil {
		return h.persistPushError(ctx, env, t, providerID, integrationID, fmt.Errorf("push: subscriber channel for %q/%q: %w", env.SubscriberID, providerID, err))
	}
	var creds struct {
		Topic string `json:"topic"`
	}
	if len(ch.Credentials) > 0 {
		if err := json.Unmarshal(ch.Credentials, &creds); err != nil {
			return h.persistPushError(ctx, env, t, providerID, integrationID, fmt.Errorf("push: unmarshal channel credentials: %w", err))
		}
	}
	if creds.Topic == "" {
		return h.persistPushError(ctx, env, t, providerID, integrationID, fmt.Errorf("push: subscriber %q has no topic for provider %q", env.SubscriberID, providerID))
	}

	if _, err := provider.Send(ctx, providers.PushMessage{
		Topic:    creds.Topic,
		Title:    subject,
		Body:     content,
		Priority: env.Step.Priority,
		Tags:     env.Step.Tags,
		Markdown: env.Step.Markdown,
	}); err != nil {
		if providers.IsTransient(err) {
			// Transient (5xx/timeout): let the engine retry the task per its
			// policy instead of recording a terminal failed message.
			return nil, engine.NewRetryableError(err)
		}
		return h.persistPushError(ctx, env, t, providerID, integrationID, err)
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
		Channel:        "push",
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
		return h.persistPushError(ctx, env, t, providerID, integrationID, err)
	}
	out, merr := json.Marshal(saved)
	if merr != nil {
		return out, merr
	}
	h.publishDelivery(env.Scope.EnvID, out)
	return out, nil
}

// persistPushSkipped records a status:"skipped" push message (the channel is
// disabled by subscriber preference) and returns its JSON with a nil error.
// No push is sent; this mirrors the existing skipped/failed-message pattern.
// resolve is attempted best-effort to populate providerID and integrationID.
func (h *PushHandler) persistPushSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	providerID, integrationID := h.resolvePushIDs(ctx, scope)
	if providerID == "" {
		providerID = "ntfy"
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

// persistPushThrottled records a status:"throttled" push message (the send
// exceeded the step's per-(env,subscriber,workflow) rate limit) and returns its
// JSON with a nil error. No push is sent; mirrors the persistPushSkipped pattern.
func (h *PushHandler) persistPushThrottled(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	providerID, integrationID := h.resolvePushIDs(ctx, scope)
	if providerID == "" {
		providerID = "ntfy"
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

// persistPushError records a status:"error" push message carrying cause and
// returns its JSON with a nil error. Best-effort: if persistence itself fails,
// the in-memory message is still marshalled and returned.
// providerID is the resolved provider on post-resolve paths and the "ntfy"
// default before resolution. integrationID is empty on pre-resolve paths and
// carries the actual value on post-resolve paths (BP-13b).
func (h *PushHandler) persistPushError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, providerID, integrationID string, cause error) (json.RawMessage, error) {
	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        "push",
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
