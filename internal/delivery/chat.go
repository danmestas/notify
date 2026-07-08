package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// ChatResolver resolves the active chat provider, its provider ID, and the
// integration ID for a scope. connectivity.Service satisfies this structurally
// (delivery does not import connectivity).
//
// Return order: (provider, providerID, integrationID, error).
// integrationID is the row's primary key, used to populate
// domain.Message.IntegrationID for Activity tab filtering.
type ChatResolver interface {
	ResolveChat(ctx context.Context, scope domain.Scope) (providers.ChatProvider, string, string, error)
}

// ChatHandler persists rendered chat messages through the store, dispatching
// via a provider resolved per scope and a per-subscriber address resolved per
// provider:
//
//   - telegram: the subscriber_channels row holds the chat_id; a subscriber
//     with no chat_id is a permanent fault (the bot cannot address them).
//   - discord:  the integration webhook IS the destination, so a missing
//     subscriber address simply falls back to the integration webhook.
type ChatHandler struct {
	store    *store.Store
	resolver ChatResolver
	prefs    PreferenceChecker
	pub      EventPublisher
	throttle Throttler
	digester Digester
}

// NewChat constructs a ChatHandler backed by s and resolver r.
func NewChat(s *store.Store, r ChatResolver) *ChatHandler {
	return &ChatHandler{store: s, resolver: r}
}

// WithPreferences attaches an optional channel-preference checker and returns
// the handler for chaining. When set, a disabled channel skips the send.
func (h *ChatHandler) WithPreferences(c PreferenceChecker) *ChatHandler {
	h.prefs = c
	return h
}

// WithEventBus attaches an optional live event publisher and returns the
// handler for chaining. When set, every terminal chat message is published as a
// delivery-status event best-effort; a nil publisher disables it.
func (h *ChatHandler) WithEventBus(p EventPublisher) *ChatHandler {
	h.pub = p
	return h
}

// WithThrottle attaches an optional per-(env,subscriber,workflow) rate limiter
// and returns the handler for chaining. When set, a step that configures a
// throttle limit records a "throttled" message instead of sending once the
// limit is exceeded; a nil throttler disables throttling.
func (h *ChatHandler) WithThrottle(t Throttler) *ChatHandler {
	h.throttle = t
	return h
}

// WithDigester attaches an optional digester and returns the handler for
// chaining. When set, a self-sourced notification carrying a DigestKey drains
// its merged events and exposes {{digest.count}} to the templates; a nil
// digester or a non-digest notification renders exactly as before.
func (h *ChatHandler) WithDigester(d Digester) *ChatHandler {
	h.digester = d
	return h
}

// publishDelivery emits a best-effort delivery-status event carrying the
// message JSON. A nil publisher or a publish failure is ignored — it must never
// fail the task.
func (h *ChatHandler) publishDelivery(envID string, data []byte) {
	if h.pub != nil {
		_ = h.pub.PublishDelivery(envID, data)
	}
}

// resolveChatIDs calls h.resolver.ResolveChat and returns the providerID and
// integrationID strings only. Used on pre-send paths (skipped, throttled) where
// we want the IDs for attribution but the provider itself is not needed. A
// resolve failure returns empty strings; callers treat "" as "unknown".
func (h *ChatHandler) resolveChatIDs(ctx context.Context, scope domain.Scope) (providerID, integrationID string) {
	_, pid, iid, err := h.resolver.ResolveChat(ctx, scope)
	if err != nil {
		return "", ""
	}
	return pid, iid
}

// chatAddress returns the per-subscriber chat address for providerID, or ""
// when the subscriber has no chat-channel row. The credentials JSON stores the
// address under "address" (Telegram chat_id, or a Discord webhook override).
func (h *ChatHandler) chatAddress(ctx context.Context, scope domain.Scope, subscriberID, providerID string) (string, error) {
	ch, err := h.store.SubscriberChannels().GetByProvider(ctx, scope, subscriberID, providerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	var creds struct {
		Address string `json:"address"`
	}
	if len(ch.Credentials) > 0 {
		if err := json.Unmarshal(ch.Credentials, &creds); err != nil {
			return "", fmt.Errorf("chat: unmarshal channel credentials: %w", err)
		}
	}
	return creds.Address, nil
}

// HandleChat is registered via engine.Handle("send.chat", h.HandleChat).
// It unmarshals the run envelope, renders the body template against the
// {subscriber, payload} context, resolves the scope's chat provider and the
// subscriber's chat address, sends the message, and persists a "sent" chat
// message. Failures are classified per 0001 §10: a transient provider fault
// (5xx, timeout) returns engine.NewRetryableError so the engine retries the
// task; every permanent fault (unmarshal, resolve, missing address, 4xx,
// persist) records an "error" message (with ErrorText) and returns its JSON
// with a nil error — a failed message, not a crashed run.
func (h *ChatHandler) HandleChat(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
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
			return h.persistChatError(ctx, env, t, "", "", err)
		}
		env = rebuilt
		digestKey = dk
		stepOutputs = so
	} else if err := json.Unmarshal(t.Input, &env); err != nil {
		return h.persistChatError(ctx, env, t, "", "", err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistChatError(ctx, env, t, "", "", err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistChatError(ctx, env, t, "", "", err)
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
	subject := template.RenderI18n(env.Step.SubjectTemplate, data, translate)
	content := template.RenderI18n(env.Step.BodyTemplate, data, translate)

	// Per-step condition gate. When the step authored a filter condition, it is
	// evaluated against the {subscriber, payload} context; a false (or
	// unevaluable) condition SKIPS the send. Absent/empty filters always run. A
	// malformed condition fails closed (skip) so a broken condition never blasts
	// a send to every subscriber.
	if !conditionMet(env.Step.Filters, subMap, payloadMap, stepOutputs) {
		return h.persistChatSkipped(ctx, env, t, scope, subject, content)
	}

	// Optional throttle gate. When the step configures a limit and a throttler
	// is attached, a send over the per-(env,subscriber,workflow) limit records a
	// "throttled" message instead of sending.
	if env.Step.ThrottleLimit > 0 && h.throttle != nil {
		key := env.Scope.EnvID + ":" + env.SubscriberID + ":" + env.WorkflowID
		allowed, _ := h.throttle.ThrottleAllow(key, env.Step.ThrottleLimit, time.Duration(env.Step.ThrottleWindowSec)*time.Second)
		if !allowed {
			return h.persistChatThrottled(ctx, env, t, scope, subject, content)
		}
	}

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), mirroring the reference default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel, env.WorkflowCritical)
		if perr == nil && !enabled {
			return h.persistChatSkipped(ctx, env, t, scope, subject, content)
		}
	}

	provider, providerID, integrationID, err := h.resolver.ResolveChat(ctx, scope)
	if err != nil {
		return h.persistChatError(ctx, env, t, "", "", err)
	}

	// Per-subscriber addressing. The chat_id (telegram) / webhook override
	// (discord) comes from the subscriber_channels row for this provider.
	// Telegram requires a chat_id; Discord falls back to the integration
	// webhook when the subscriber has no address.
	address, err := h.chatAddress(ctx, scope, env.SubscriberID, providerID)
	if err != nil {
		return h.persistChatError(ctx, env, t, providerID, integrationID, fmt.Errorf("chat: subscriber channel for %q/%q: %w", env.SubscriberID, providerID, err))
	}
	if providerID == "telegram" && address == "" {
		return h.persistChatError(ctx, env, t, providerID, integrationID, fmt.Errorf("chat: subscriber %q has no chat_id for provider %q", env.SubscriberID, providerID))
	}

	if _, err := provider.Send(ctx, providers.ChatMessage{
		Address: address,
		Content: content,
	}); err != nil {
		if providers.IsTransient(err) {
			// Transient (5xx/timeout): let the engine retry the task per its
			// policy instead of recording a terminal failed message.
			return nil, engine.NewRetryableError(err)
		}
		return h.persistChatError(ctx, env, t, providerID, integrationID, err)
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
		Channel:        "chat",
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
		return h.persistChatError(ctx, env, t, providerID, integrationID, err)
	}
	out, merr := json.Marshal(saved)
	if merr != nil {
		return out, merr
	}
	h.publishDelivery(env.Scope.EnvID, out)
	return out, nil
}

// persistChatSkipped records a status:"skipped" chat message (the channel is
// disabled by subscriber preference) and returns its JSON with a nil error. No
// chat is sent; this mirrors the existing skipped/failed-message pattern.
func (h *ChatHandler) persistChatSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	providerID, integrationID := h.resolveChatIDs(ctx, scope)
	if providerID == "" {
		providerID = "discord" // safe default; resolve failure must not block the record
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

// persistChatThrottled records a status:"throttled" chat message (the send
// exceeded the step's per-(env,subscriber,workflow) rate limit) and returns its
// JSON with a nil error. No chat is sent; mirrors the persistChatSkipped pattern.
func (h *ChatHandler) persistChatThrottled(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, scope domain.Scope, subject, content string) (json.RawMessage, error) {
	providerID, integrationID := h.resolveChatIDs(ctx, scope)
	if providerID == "" {
		providerID = "discord"
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

// persistChatError records a status:"error" chat message carrying cause and
// returns its JSON with a nil error. Best-effort: if persistence itself fails,
// the in-memory message is still marshalled and returned. providerID and
// integrationID are empty on pre-resolve paths and carry the actual values on
// post-resolve paths (send failure, persist failure).
func (h *ChatHandler) persistChatError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, providerID, integrationID string, cause error) (json.RawMessage, error) {
	if providerID == "" {
		providerID = "discord" // safe default for pre-resolve error rows
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
		Channel:        "chat",
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
