package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/metrics"
	"github.com/danmestas/notify/internal/providers"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// webPushProviderID is the resolved provider id that routes HandlePush
// through the per-device webpush fan-out instead of the single
// subscriber_channels-addressed send every other push provider uses.
const webPushProviderID = "webpush"

// webPushSender is the subset of *webpushprovider.Client the fan-out needs.
// Declared narrow (rather than importing the concrete type at every call
// site) so the type assertion below stays the only coupling point.
type webPushSender interface {
	SendToSubscription(ctx context.Context, sub webpushprovider.Subscription, msg providers.PushMessage) (providers.Result, error)
}

// PushResolver resolves the active push provider, provider ID, and integration
// ID for a scope. connectivity.Service satisfies this structurally (delivery
// does not import connectivity).
//
// Return order: (provider, providerID, integrationID, error).
// integrationID is the row's primary key, used to populate
// domain.Message.IntegrationID for Activity tab filtering (BP-13b).
//
// ResolveIntegration resolves the provider, providerID, and integrationID for
// a specific integration identifier within (scope, "push"), bypassing the
// primary-integration resolution. connectivity.Service.ResolveIntegration
// satisfies this. The provider is untyped (any) because the four channels
// have no shared provider interface; HandlePush type-asserts to
// providers.PushProvider.
type PushResolver interface {
	ResolvePush(ctx context.Context, scope domain.Scope) (providers.PushProvider, string, string, error)
	ResolveIntegration(ctx context.Context, scope domain.Scope, channel, identifier string) (any, string, string, error)
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
	// webPushRetryDelay is how long a Web Push send waits before its one
	// retry of devices that failed transiently. A field so tests need not
	// sleep for the production value.
	webPushRetryDelay time.Duration
}

// NewPush constructs a PushHandler backed by s and resolver r.
func NewPush(s *store.Store, r PushResolver) *PushHandler {
	return &PushHandler{store: s, resolver: r, webPushRetryDelay: webPushRetryDelay}
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
func (h *PushHandler) resolvePushIDs(ctx context.Context, scope domain.Scope, identifier string) (providerID, integrationID string) {
	if identifier != "" {
		_, pid, iid, err := h.resolver.ResolveIntegration(ctx, scope, "push", identifier)
		if err != nil {
			return "", ""
		}
		return pid, iid
	}
	_, pid, iid, err := h.resolver.ResolvePush(ctx, scope)
	if err != nil {
		return "", ""
	}
	return pid, iid
}

// resolvePushProvider resolves the PushProvider, providerID, and
// integrationID for the send, routing through ResolveIntegration when the
// step sets an explicit "integration" identifier and through the
// primary-resolution path (ResolvePush) otherwise — byte-identical to
// pre-#314 behavior when unset.
func (h *PushHandler) resolvePushProvider(ctx context.Context, scope domain.Scope, identifier string) (providers.PushProvider, string, string, error) {
	if identifier == "" {
		return h.resolver.ResolvePush(ctx, scope)
	}
	p, providerID, integrationID, err := h.resolver.ResolveIntegration(ctx, scope, "push", identifier)
	if err != nil {
		return nil, "", "", err
	}
	pp, ok := p.(providers.PushProvider)
	if !ok {
		return nil, providerID, integrationID, fmt.Errorf("push: integration %q resolved a non-push provider (%T)", identifier, p)
	}
	return pp, providerID, integrationID, nil
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
	var stepOutputs map[string]map[string]any
	if len(t.Input) == 0 || !isRunEnvelope(t.Input) {
		// Self-source: an empty input means an upstream step (e.g. a delay)
		// produced no output; a non-envelope input means an upstream action step
		// (e.g. http_request) produced non-envelope output. Either way, rebuild
		// the envelope from the store by run id.
		rebuilt, dk, so, err := RebuildEnvelope(ctx, h.store, t)
		if err != nil {
			return h.persistPushError(ctx, env, t, "ntfy", "", err)
		}
		env = rebuilt
		digestKey = dk
		stepOutputs = so
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

	data := map[string]any{"subscriber": subMap, "payload": payloadMap, "steps": stepOutputs}
	// Digest aggregate: a self-sourced notification with a DigestKey drains the
	// merged events so the body can render {{digest.count}}. Only the
	// self-source path sets digestKey, so the Input-present path is unchanged.
	if digestKey != "" && h.digester != nil {
		events, _ := h.digester.DigestDrain(digestKey)
		data["digest"] = digestData(events)
	}
	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
	translate := storeTranslator(ctx, h.store, scope, subMap)
	subject := template.RenderAs(template.Plain, env.Step.SubjectTemplate, data, translate)
	content := template.RenderAs(template.Plain, env.Step.BodyTemplate, data, translate)
	// Step.Data is authored in the same editor and the same {{path}} namespace
	// as subject and body -- {"url":"{{payload.link}}"} is the common case, a
	// per-notification deep link -- so it renders here, once, for every send
	// path below (ntfy at the end of this function, web push via
	// handleWebPushSend). Left unrendered it reaches the device as the literal
	// template and the deep link is dead (#328). A NEW map is assigned rather
	// than the existing one mutated, so the authored templates in the envelope
	// are never overwritten in place.
	env.Step.Data = renderPushData(env.Step.Data, func(tmpl string) string {
		return template.RenderAs(template.Plain, tmpl, data, translate)
	})

	// Per-step condition gate. When the step authored a filter condition, it is
	// evaluated against the {subscriber, payload} context; a false (or
	// unevaluable) condition SKIPS the send. Absent/empty filters always run. A
	// malformed condition fails closed (skip) so a broken condition never blasts
	// a send to every subscriber.
	if !conditionMet(env.Step.Filters, subMap, payloadMap, stepOutputs) {
		return h.persistPushSkipped(ctx, env, t, scope, subject, content)
	}

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
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel, env.WorkflowCritical)
		if perr == nil && !enabled {
			return h.persistPushSkipped(ctx, env, t, scope, subject, content)
		}
	}

	provider, providerID, integrationID, err := h.resolvePushProvider(ctx, scope, env.Step.Integration)
	if err != nil {
		return h.persistPushError(ctx, env, t, "ntfy", "", err)
	}

	// Web Push fans out to every device subscription registered for the
	// subscriber instead of the single subscriber_channels-addressed send
	// every other push provider uses — see handleWebPushSend.
	if providerID == webPushProviderID {
		return h.handleWebPushSend(ctx, env, t, scope, provider, integrationID, subject, content)
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
		Data:     env.Step.Data,
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
	metrics.Delivery("push", metrics.OutcomeDelivered)
	out, merr := json.Marshal(saved)
	if merr != nil {
		return out, merr
	}
	h.publishDelivery(env.Scope.EnvID, out)
	return out, nil
}

// handleWebPushSend fans the rendered subject/content out to every device
// subscription webpush_subscriptions holds for env.SubscriberID: one
// SendToSubscription call per row (there is no single "topic" to address, so
// this bypasses the subscriber_channels lookup entirely). A subscriber with
// no rows is skipped (mirrors the missing-address path). Per row: a 404/410
// (webpushprovider.ErrSubscriptionGone) deletes that row and continues,
// anything else counts as a failed attempt and delivery continues to the next
// row. The persisted message is "sent" when at least one row delivered,
// "error" when every row failed with at least one permanent (non-transient)
// cause. When every row fails and every failure is transient (5xx/timeout —
// providers.IsTransient), nothing was delivered to anyone, so it is safe to
// let the engine retry the whole step: this returns engine.NewRetryableError
// instead of persisting a terminal message, same as the single-address path
// below. A retry never re-sends to a device that already succeeded, because
// sent == 0 is required to take this branch.
func (h *PushHandler) handleWebPushSend(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, provider providers.PushProvider, integrationID, subject, content string) (json.RawMessage, error) {
	sender, ok := provider.(webPushSender)
	if !ok {
		return h.persistPushError(ctx, env, t, webPushProviderID, integrationID, fmt.Errorf("webpush: resolved provider %T does not support per-device delivery", provider))
	}

	subs, err := h.store.WebPushSubscriptions().ListBySubscriber(ctx, scope, env.SubscriberID)
	if err != nil {
		return h.persistPushError(ctx, env, t, webPushProviderID, integrationID, fmt.Errorf("webpush: list subscriptions: %w", err))
	}
	if len(subs) == 0 {
		return h.persistPushSkipped(ctx, env, t, scope, subject, content)
	}

	msg := providers.PushMessage{
		Title:    subject,
		Body:     content,
		Priority: env.Step.Priority,
		Tags:     env.Step.Tags,
		Markdown: env.Step.Markdown,
		Data:     env.Step.Data,
	}

	outcomes, sent, failed, lastErr := h.fanOutWebPush(ctx, scope, env.SubscriberID, sender, subs, msg)

	if sent == 0 {
		if lastErr == nil {
			// Every subscription was gone (pruned): nothing left to address,
			// same outcome as no rows at all.
			return h.persistPushSkipped(ctx, env, t, scope, subject, content)
		}
		if providers.IsTransient(lastErr) {
			// Every attempt failed transiently (5xx/timeout) and nothing was
			// delivered: retry the whole step instead of recording a terminal
			// failure, matching the single-address path's classification. A
			// retry never re-sends to a device that already succeeded,
			// because sent == 0 is required to take this branch.
			return nil, engine.NewRetryableError(fmt.Errorf("webpush: 0/%d subscriptions delivered: %w", len(subs), lastErr))
		}
		extras, _ := json.Marshal(map[string]any{"devices": outcomes})
		return h.persistPushErrorWithExtras(ctx, env, t, webPushProviderID, integrationID, fmt.Errorf("webpush: 0/%d subscriptions delivered: %w", len(subs), lastErr), extras)
	}

	// At least one device delivered, so the message is "sent". A partial
	// failure must not be invisible: the tally goes in ErrorText, and every
	// device's outcome in extras.devices, so a client can show which device
	// refused and why (#359).
	var errorText string
	if failed > 0 {
		errorText = fmt.Sprintf("delivered %d/%d: %d failure(s) (%s)", sent, sent+failed, failed, lastErr)
	}
	extras, _ := json.Marshal(map[string]any{"devices": outcomes})

	saved := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        "push",
		ProviderID:     webPushProviderID,
		IntegrationID:  integrationID,
		Content:        content,
		Subject:        subject,
		Status:         "sent",
		ErrorText:      errorText,
		Seen:           false,
		Read:           false,
		Archived:       false,
		TransactionID:  env.TransactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Extras:         extras,
	}
	persisted, err := h.store.Messages().Create(ctx, scope, saved)
	if err != nil {
		return h.persistPushError(ctx, env, t, webPushProviderID, integrationID, err)
	}
	metrics.Delivery("push", metrics.OutcomeDelivered)
	out, merr := json.Marshal(persisted)
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
	metrics.Delivery("push", metrics.OutcomeSkipped)
	providerID, integrationID := h.resolvePushIDs(ctx, scope, env.Step.Integration)
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
	metrics.Delivery("push", metrics.OutcomeThrottled)
	providerID, integrationID := h.resolvePushIDs(ctx, scope, env.Step.Integration)
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
	return h.persistPushErrorWithExtras(ctx, env, t, providerID, integrationID, cause, nil)
}

// persistPushErrorWithExtras is persistPushError with the message's extras
// set: a Web Push send that reached no device still records each device's
// outcome, since that is when a client most needs to know which refused
// and why (#370).
func (h *PushHandler) persistPushErrorWithExtras(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, providerID, integrationID string, cause error, extras json.RawMessage) (json.RawMessage, error) {
	metrics.Delivery("push", metrics.OutcomeError)
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
		Extras:         extras,
	}
	saved, err := h.store.Messages().Create(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, msg)
	if err != nil {
		saved = msg
	}
	out, merr := json.Marshal(saved)
	h.publishDelivery(env.Scope.EnvID, out)
	return out, merr
}

// renderPushData returns a new map with every value rendered. Keys are left
// alone: they are the receiving app's contract for reading the data payload,
// not authored copy. A nil or empty map renders to nil so no data payload is
// sent, exactly as before.
func renderPushData(in map[string]string, render func(string) string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = render(v)
	}
	return out
}
