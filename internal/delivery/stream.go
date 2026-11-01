package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// FeedPublisher publishes a rendered stream event to the durable env feed.
// eventbus.Bus satisfies this structurally (delivery does not import
// eventbus). A nil publisher means "no live feed publish" (the message is
// still persisted; only the durable-feed side effect is skipped).
type FeedPublisher interface {
	PublishFeed(environmentID, topic string, data []byte) error
}

// StreamHandler persists rendered stream-channel messages through the store
// and publishes them to the environment's durable feed. It is a separate
// handler (like PushHandler/EmailHandler), not a method on the shared
// Handler, because it carries a dependency (FeedPublisher) none of the other
// channels need.
type StreamHandler struct {
	store    *store.Store
	prefs    PreferenceChecker
	pub      EventPublisher
	feed     FeedPublisher
	throttle Throttler
	digester Digester
}

// NewStream constructs a StreamHandler backed by s.
func NewStream(s *store.Store) *StreamHandler {
	return &StreamHandler{store: s}
}

// WithPreferences attaches an optional channel-preference checker and returns
// the handler for chaining. When set, a disabled channel skips the send.
func (h *StreamHandler) WithPreferences(c PreferenceChecker) *StreamHandler {
	h.prefs = c
	return h
}

// WithEventBus attaches an optional live event publisher and returns the
// handler for chaining. When set, every terminal stream message is published
// as a delivery-status event best-effort; a nil publisher disables it.
func (h *StreamHandler) WithEventBus(p EventPublisher) *StreamHandler {
	h.pub = p
	return h
}

// WithFeed attaches an optional durable-feed publisher and returns the
// handler for chaining. When set, a sent stream message is published to the
// environment's feed topic best-effort; a nil publisher disables it (the
// message is still persisted).
func (h *StreamHandler) WithFeed(f FeedPublisher) *StreamHandler {
	h.feed = f
	return h
}

// WithThrottle attaches an optional per-(env,subscriber,workflow) rate limiter
// and returns the handler for chaining. When set, a step that configures a
// throttle limit records a "throttled" message instead of sending once the
// limit is exceeded; a nil throttler disables throttling.
func (h *StreamHandler) WithThrottle(t Throttler) *StreamHandler {
	h.throttle = t
	return h
}

// WithDigester attaches an optional digester and returns the handler for
// chaining. When set, a self-sourced notification carrying a DigestKey drains
// its merged events and exposes {{digest.count}} to the templates; a nil
// digester or a non-digest notification renders exactly as before.
func (h *StreamHandler) WithDigester(d Digester) *StreamHandler {
	h.digester = d
	return h
}

// publishDelivery emits a best-effort delivery-status event carrying the
// message JSON. A nil publisher or a publish failure is ignored — it must
// never fail the task.
func (h *StreamHandler) publishDelivery(envID string, data []byte) {
	if h.pub != nil {
		_ = h.pub.PublishDelivery(envID, data)
	}
}

// HandleStream is registered via engine.Handle("notify.send.stream", h.HandleStream).
// It mirrors Handler.HandleInApp's control flow (self-source rebuild, render,
// filter/throttle/preference gates, persist, best-effort live publish) with
// two deltas: the routing key is a rendered "topic" (validated as a hard,
// fail-closed gate before anything else renders or gates run) rather than a
// subject/body pair, and a "sent" message publishes to the durable feed
// instead of the inbox.
func (h *StreamHandler) HandleStream(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var env runenv.InAppEnvelope
	var digestKey string
	var stepOutputs map[string]map[string]any
	if len(t.Input) == 0 || !isRunEnvelope(t.Input) {
		// Self-source: an empty input means an upstream step (e.g. a delay)
		// produced no output, and a non-envelope input means an upstream action
		// step (e.g. http_request) produced output that is not an InAppEnvelope.
		// Either way, rebuild the envelope from the store by run id.
		rebuilt, dk, so, err := RebuildEnvelope(ctx, h.store, t)
		if err != nil {
			return h.persistStreamError(ctx, env, t, err)
		}
		env = rebuilt
		digestKey = dk
		stepOutputs = so
	} else if err := json.Unmarshal(t.Input, &env); err != nil {
		// Envelope undecodable: scope/subscriber are unknown (empty); still
		// record a failed message so the failure is observable.
		return h.persistStreamError(ctx, env, t, err)
	}

	subMap := map[string]any{}
	if len(env.Subscriber) > 0 {
		if err := json.Unmarshal(env.Subscriber, &subMap); err != nil {
			return h.persistStreamError(ctx, env, t, err)
		}
	}
	payloadMap := map[string]any{}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payloadMap); err != nil {
			return h.persistStreamError(ctx, env, t, err)
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

	// Render the routing key first and hard-validate it before any body
	// render or gate runs: an injected or malformed rendered topic must fail
	// the send closed, observably, rather than reach PublishFeed (which
	// re-validates too, but only as defense in depth — this is the primary
	// gate).
	topic := template.RenderAs(template.Plain, env.Step.Topic, data, translate)
	if err := domain.ValidateFeedTopic(topic); err != nil {
		return h.persistStreamError(ctx, env, t, fmt.Errorf("stream: invalid topic after render: %w", err))
	}
	content := template.RenderAs(template.Plain, env.Step.BodyTemplate, data, translate)

	// Per-step condition gate. When the step authored a filter condition, it is
	// evaluated against the {subscriber, payload} context; a false (or
	// unevaluable) condition SKIPS the send. Absent/empty filters always run. A
	// malformed condition fails closed (skip) so a broken condition never blasts
	// a send to every subscriber.
	if !conditionMet(env.Step.Filters, subMap, payloadMap, stepOutputs) {
		return h.persistStreamSkipped(ctx, env, t, topic, content)
	}

	// Optional throttle gate. When the step configures a limit and a throttler
	// is attached, a send over the per-(env,subscriber,workflow) limit records a
	// "throttled" message instead of sending.
	if env.Step.ThrottleLimit > 0 && h.throttle != nil {
		key := env.Scope.EnvID + ":" + env.SubscriberID + ":" + env.WorkflowID
		allowed, _ := h.throttle.ThrottleAllow(key, env.Step.ThrottleLimit, time.Duration(env.Step.ThrottleWindowSec)*time.Second)
		if !allowed {
			return h.persistStreamThrottled(ctx, env, t, topic, content)
		}
	}

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), a default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel, env.WorkflowCritical)
		if perr == nil && !enabled {
			return h.persistStreamSkipped(ctx, env, t, topic, content)
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
		Channel:        "stream",
		ProviderID:     env.Step.ProviderID,
		Content:        content,
		// Subject carries the rendered topic — stream has no separate subject
		// template — so console/analytics message listings can show the
		// routing key without a schema change.
		Subject:       topic,
		Status:        "sent",
		Seen:          false,
		Read:          false,
		Archived:      false,
		TransactionID: env.TransactionID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	saved, err := h.store.Messages().Create(ctx, scope, msg)
	if err != nil {
		return h.persistStreamError(ctx, env, t, err)
	}
	out, err := json.Marshal(saved)
	if err != nil {
		return out, err
	}
	// Best-effort live publishes for the sent message. A publish failure must
	// not fail the task — the message is already persisted. The feed publish
	// carries the message to the durable env feed (SSE bridge readers); the
	// delivery-status event drives outbound webhooks/analytics. No
	// PublishInbox: a stream message is not an inbox message.
	if h.feed != nil {
		_ = h.feed.PublishFeed(env.Scope.EnvID, topic, out)
	}
	h.publishDelivery(env.Scope.EnvID, out)
	return out, nil
}

// persistStreamSkipped records a status:"skipped" stream message (the channel
// is disabled by subscriber preference, or the step's filter condition
// evaluated false) and returns its JSON with a nil error. No feed publish
// occurs; this mirrors Handler.persistSkipped.
func (h *StreamHandler) persistStreamSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, topic, content string) (json.RawMessage, error) {
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
		Subject:        topic,
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

// persistStreamThrottled records a status:"throttled" stream message (the
// send exceeded the step's per-(env,subscriber,workflow) rate limit) and
// returns its JSON with a nil error. No feed publish occurs; mirrors
// persistStreamSkipped.
func (h *StreamHandler) persistStreamThrottled(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, topic, content string) (json.RawMessage, error) {
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
		Subject:        topic,
		Status:         "throttled",
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

// persistStreamError records a status:"error" stream message carrying cause
// (e.g. an undecodable envelope or an injected/malformed rendered topic) and
// returns its JSON with a nil error. Best-effort: if persistence itself
// fails, the in-memory message is still marshalled and returned so the
// caller observes the failure. No feed publish occurs.
func (h *StreamHandler) persistStreamError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, cause error) (json.RawMessage, error) {
	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          env.Scope.OrgID,
		EnvID:          env.Scope.EnvID,
		WorkflowID:     env.WorkflowID,
		NotificationID: env.NotificationID,
		RunID:          t.RunID,
		StepID:         env.Step.StepID,
		SubscriberID:   env.SubscriberID,
		Channel:        "stream",
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
