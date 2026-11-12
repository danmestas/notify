package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/metrics"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// SMSResolver resolves the active SMS provider, its provider ID, and the
// integration ID for a scope. connectivity.Service satisfies this structurally
// (delivery does not import connectivity).
//
// Return order: (provider, providerID, integrationID, error).
// integrationID is the row's primary key, used to populate
// domain.Message.IntegrationID for Activity tab filtering.
//
// ResolveIntegration resolves the provider, providerID, and integrationID for
// a specific integration identifier within (scope, "sms"), bypassing the
// primary-integration resolution. connectivity.Service.ResolveIntegration
// satisfies this. The provider is untyped (any) because the four channels
// have no shared provider interface; HandleSMS type-asserts to
// providers.SMSProvider.
type SMSResolver interface {
	ResolveSMS(ctx context.Context, scope domain.Scope) (providers.SMSProvider, string, string, error)
	ResolveIntegration(ctx context.Context, scope domain.Scope, channel, identifier string) (any, string, string, error)
}

// SMSHandler persists rendered SMS messages through the store, dispatching via a
// provider resolved per scope. The recipient is the subscriber's verified phone,
// read from the store at send time: a phone is verified only by the subscriber
// (internal/phoneverify), so a subscriber with none, or with an unverified one,
// has the step skipped (SkipReasonNoVerifiedPhone) — not a fault.
type SMSHandler struct {
	store    *store.Store
	resolver SMSResolver
	prefs    PreferenceChecker
	pub      EventPublisher
	throttle Throttler
	digester Digester
	// statusCallbackBase is notify's public URL (origin plus base path), or
	// "" when it has none.
	statusCallbackBase string
}

// NewSMS constructs an SMSHandler backed by s and resolver r.
func NewSMS(s *store.Store, r SMSResolver) *SMSHandler {
	return &SMSHandler{store: s, resolver: r}
}

// WithPreferences attaches an optional channel-preference checker and returns
// the handler for chaining. When set, a disabled channel skips the send.
func (h *SMSHandler) WithPreferences(c PreferenceChecker) *SMSHandler {
	h.prefs = c
	return h
}

// WithEventBus attaches an optional live event publisher and returns the
// handler for chaining. When set, every terminal SMS message is published as a
// delivery-status event best-effort; a nil publisher disables it.
func (h *SMSHandler) WithEventBus(p EventPublisher) *SMSHandler {
	h.pub = p
	return h
}

// WithThrottle attaches an optional per-(env,subscriber,workflow) rate limiter
// and returns the handler for chaining. When set, a step that configures a
// throttle limit records a "throttled" message instead of sending once the
// limit is exceeded; a nil throttler disables throttling.
func (h *SMSHandler) WithThrottle(t Throttler) *SMSHandler {
	h.throttle = t
	return h
}

// WithStatusCallbackBase sets notify's public URL (origin plus base path).
// With it set, every Twilio or SignalWire send asks the provider to report
// the message's delivery status to that integration's status route
// (/v1/inbound/sms/{integrationId}/status); a number's own settings are not
// relied on. Empty asks for none.
func (h *SMSHandler) WithStatusCallbackBase(base string) *SMSHandler {
	h.statusCallbackBase = strings.TrimRight(base, "/")
	return h
}

// statusCallback is the status route for integrationID, or "" when notify
// has no public URL or the provider sends no status callbacks there.
func (h *SMSHandler) statusCallback(providerID, integrationID string) string {
	if h.statusCallbackBase == "" || integrationID == "" || (providerID != "twilio" && providerID != "signalwire") {
		return ""
	}
	return h.statusCallbackBase + "/v1/inbound/sms/" + integrationID + "/status"
}

// WithDigester attaches an optional digester and returns the handler for
// chaining. When set, a self-sourced notification carrying a DigestKey drains
// its merged events and exposes {{digest.count}} to the templates; a nil
// digester or a non-digest notification renders exactly as before.
func (h *SMSHandler) WithDigester(d Digester) *SMSHandler {
	h.digester = d
	return h
}

// publishDelivery emits a best-effort delivery-status event carrying the
// message JSON. A nil publisher or a publish failure is ignored — it must never
// fail the task.
func (h *SMSHandler) publishDelivery(envID string, data []byte) {
	if h.pub != nil {
		_ = h.pub.PublishDelivery(envID, data)
	}
}

// resolveSMSIDs resolves the providerID and integrationID strings only,
// honoring an explicit step-level integration identifier when set. Used on
// pre-send paths (skipped, throttled) where we want the IDs for attribution
// but the provider itself is not needed. A resolve failure returns empty
// strings; callers treat "" as "unknown".
func (h *SMSHandler) resolveSMSIDs(ctx context.Context, scope domain.Scope, identifier string) (providerID, integrationID string) {
	if identifier != "" {
		_, pid, iid, err := h.resolver.ResolveIntegration(ctx, scope, "sms", identifier)
		if err != nil {
			return "", ""
		}
		return pid, iid
	}
	_, pid, iid, err := h.resolver.ResolveSMS(ctx, scope)
	if err != nil {
		return "", ""
	}
	return pid, iid
}

// resolveSMSProvider resolves the SMSProvider, providerID, and integrationID
// for the send, routing through ResolveIntegration when the step sets an
// explicit "integration" identifier and through the primary-resolution path
// (ResolveSMS) otherwise — byte-identical to pre-#314 behavior when unset.
func (h *SMSHandler) resolveSMSProvider(ctx context.Context, scope domain.Scope, identifier string) (providers.SMSProvider, string, string, error) {
	if identifier == "" {
		return h.resolver.ResolveSMS(ctx, scope)
	}
	p, providerID, integrationID, err := h.resolver.ResolveIntegration(ctx, scope, "sms", identifier)
	if err != nil {
		return nil, "", "", err
	}
	sp, ok := p.(providers.SMSProvider)
	if !ok {
		return nil, providerID, integrationID, fmt.Errorf("sms: integration %q resolved a non-sms provider (%T)", identifier, p)
	}
	return sp, providerID, integrationID, nil
}

// HandleSMS is registered via engine.Handle("notify.send.sms", h.HandleSMS).
// It unmarshals the run envelope, renders the body template against the
// {subscriber, payload} context, resolves the subscriber's verified phone (a
// subscriber with none is skipped) and the scope's SMS provider, sends the
// message, and persists a "sent" sms message. Failures are classified per 0001
// §10: a transient provider fault (5xx, timeout) returns
// engine.NewRetryableError so the engine retries the task; every permanent
// fault (unmarshal, resolve, 4xx, persist) records an "error" message (with
// ErrorText) and returns its JSON with a nil error — a failed message, not a
// crashed run.
func (h *SMSHandler) HandleSMS(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	env, digestKey, stepOutputs, err := taskEnvelope(ctx, h.store, t)
	if err != nil {
		return h.persistSMSError(ctx, env, t, "", "", err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistSMSError(ctx, env, t, "", "", err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistSMSError(ctx, env, t, "", "", err)
		}
	}

	data := map[string]any{"subscriber": subMap, "payload": payloadMap, "steps": stepOutputs}
	// Digest aggregate: a self-sourced notification with a DigestKey drains the
	// merged events so the body can render {{digest.count}}. Only the
	// self-source path sets digestKey, so the Input-present path is unchanged.
	if digestKey != "" && h.digester != nil {
		events, _ := h.digester.DigestBatch(digestKey, t.RunID)
		data["digest"] = digestData(events)
	}
	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
	translate := storeTranslator(ctx, h.store, scope, subMap)
	subject := template.RenderAs(template.Plain, env.Step.SubjectTemplate, data, translate)
	content := template.RenderAs(template.Plain, env.Step.BodyTemplate, data, translate)

	// Per-step condition gate. When the step authored a filter condition, it is
	// evaluated against the {subscriber, payload} context; a false (or
	// unevaluable) condition SKIPS the send. Absent/empty filters always run. A
	// malformed condition fails closed (skip) so a broken condition never blasts
	// a send to every subscriber.
	if met, invalid := conditionOutcome(env.Step.Filters, subMap, payloadMap, stepOutputs); !met {
		reason := SkipReasonCondition
		if invalid {
			reason = SkipReasonConditionInvalid
		}
		return h.persistSMSSkipped(ctx, env, t, scope, subject, content, reason)
	}

	// Optional throttle gate. When the step configures a limit and a throttler
	// is attached, a send over the per-(env,subscriber,workflow) limit records a
	// "throttled" message instead of sending.
	if env.Step.ThrottleLimit > 0 && h.throttle != nil {
		key := env.Scope.EnvID + ":" + env.SubscriberID + ":" + env.WorkflowID
		allowed, _ := h.throttle.ThrottleAllow(key, env.Step.ThrottleLimit, time.Duration(env.Step.ThrottleWindowSec)*time.Second)
		if !allowed {
			return h.persistSMSThrottled(ctx, env, t, scope, subject, content)
		}
	}

	// A number that texted STOP gets nothing, whatever the preferences or the
	// workflow's criticality say: the opt-out is the sender's legal duty, not
	// a preference. It is checked first so the skip says why.
	optedOut, err := h.optedOut(ctx, scope, env.SubscriberID)
	if err != nil {
		return h.persistSMSError(ctx, env, t, "", "", err)
	}
	if optedOut {
		return h.persistSMSSkipped(ctx, env, t, scope, subject, content, SkipReasonOptedOut)
	}

	// Optional preference filtering. A nil checker always sends. With no
	// subscriber preference the step's own default decides, and SMS is off
	// unless the workflow turns it on (env.Step.DefaultEnabled). On a checker
	// error we fail open (proceed to send) rather than dropping the
	// notification on a transient lookup fault; the verified-phone gate below
	// still applies.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel, env.WorkflowCritical, env.Step.DefaultEnabled)
		if perr == nil && !enabled {
			return h.persistSMSSkipped(ctx, env, t, scope, subject, content, SkipReasonPreference)
		}
	}

	// SMS goes only to a phone the subscriber verified. It is read from the
	// store, not the trigger-time snapshot, so a number changed (and so
	// unverified) since the trigger is never sent to.
	to, err := h.verifiedPhone(ctx, scope, env.SubscriberID)
	if err != nil {
		return h.persistSMSError(ctx, env, t, "", "", err)
	}
	if to == "" {
		return h.persistSMSSkipped(ctx, env, t, scope, subject, content, SkipReasonNoVerifiedPhone)
	}

	provider, providerID, integrationID, err := h.resolveSMSProvider(ctx, scope, env.Step.Integration)
	if err != nil {
		return h.persistSMSError(ctx, env, t, "", "", resolveFailure("sms", err))
	}

	res, err := provider.Send(ctx, providers.SMSMessage{
		To:             to,
		Body:           content,
		StatusCallback: h.statusCallback(providerID, integrationID),
	})
	if err != nil {
		if providers.IsTransient(err) {
			// Transient (5xx/timeout): let the engine retry the task per its
			// policy instead of recording a terminal failed message.
			return nil, retryTransient(ctx, h.store, scope, integrationID, err)
		}
		return h.persistSMSError(ctx, env, t, providerID, integrationID, err)
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
		Channel:        "sms",
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
		// A delivery-status callback finds the message by this id.
		ProviderMessageID: res.ProviderMessageID,
	}

	saved, err := h.store.Messages().Create(ctx, scope, msg)
	if err != nil {
		return h.persistSMSError(ctx, env, t, providerID, integrationID, err)
	}
	metrics.Delivery("sms", metrics.OutcomeDelivered)
	out, merr := json.Marshal(saved)
	if merr != nil {
		return out, merr
	}
	h.publishDelivery(env.Scope.EnvID, out)
	return out, nil
}

// optedOut reports whether the subscriber's phone, verified or not, texted
// STOP. A subscriber with no phone, or none stored, is not opted out.
func (h *SMSHandler) optedOut(ctx context.Context, scope domain.Scope, subscriberID string) (bool, error) {
	sub, err := h.store.Subscribers().GetByExternalID(ctx, scope, subscriberID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sms: load subscriber: %w", err)
	}
	if sub.Phone == "" {
		return false, nil
	}
	out, err := h.store.SMSOptOuts().IsOptedOut(ctx, scope, sub.Phone)
	if err != nil {
		return false, fmt.Errorf("sms: opt-out lookup: %w", err)
	}
	return out, nil
}

// verifiedPhone returns the subscriber's verified phone, or "" when they have
// none (no phone, an unverified one, or no stored subscriber at all).
func (h *SMSHandler) verifiedPhone(ctx context.Context, scope domain.Scope, subscriberID string) (string, error) {
	sub, err := h.store.Subscribers().GetByExternalID(ctx, scope, subscriberID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sms: load subscriber: %w", err)
	}
	if sub.Deleted || sub.PhoneVerifiedAt == "" {
		return "", nil
	}
	return sub.Phone, nil
}

// persistSMSSkipped records a status:"skipped" sms message carrying the gate
// that skipped it (reason) and returns its JSON with a nil error. No SMS is
// sent; this mirrors the existing skipped/failed-message pattern.
func (h *SMSHandler) persistSMSSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content, reason string) (json.RawMessage, error) {
	metrics.Delivery("sms", metrics.OutcomeSkipped)
	providerID, integrationID := h.resolveSMSIDs(ctx, scope, env.Step.Integration)
	if providerID == "" {
		providerID = "twilio" // safe default; resolve failure must not block the record
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
		SkipReason:     reason,
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

// persistSMSThrottled records a status:"throttled" sms message (the send
// exceeded the step's per-(env,subscriber,workflow) rate limit) and returns its
// JSON with a nil error. No SMS is sent; mirrors the persistSMSSkipped pattern.
func (h *SMSHandler) persistSMSThrottled(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	metrics.Delivery("sms", metrics.OutcomeThrottled)
	providerID, integrationID := h.resolveSMSIDs(ctx, scope, env.Step.Integration)
	if providerID == "" {
		providerID = "twilio"
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

// persistSMSError records a status:"error" sms message carrying cause and
// returns its JSON with a nil error. Best-effort: if persistence itself fails,
// the in-memory message is still marshalled and returned. providerID and
// integrationID are empty on pre-resolve paths and carry the actual values on
// post-resolve paths (send failure, persist failure).
func (h *SMSHandler) persistSMSError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, providerID, integrationID string, cause error) (json.RawMessage, error) {
	metrics.Delivery("sms", metrics.OutcomeError)
	if providerID == "" {
		providerID = "twilio" // safe default for pre-resolve error rows
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
		Channel:        "sms",
		ProviderID:     providerID,
		IntegrationID:  integrationID,
		Status:         "error",
		ErrorText:      cause.Error(),
		ErrorClass:     string(providers.ClassifyError(cause)),
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
