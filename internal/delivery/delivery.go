// Package delivery implements channel send handlers registered with the engine.
// HandleInApp is the send.in_app task handler: it renders the authored subject
// and body templates against the run envelope and persists the resulting in-app
// message. mirrors the reference behaviour of recording a failed message (status
// "error") rather than crashing the run when the envelope is unusable.
package delivery

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/metrics"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// PreferenceChecker reports whether a channel is enabled for a subscriber/workflow.
// preferences.Service satisfies this structurally (delivery does not import
// preferences). A nil checker means "always send" (no preference filtering).
// critical is the triggering workflow's Critical flag (env.WorkflowCritical);
// every call site forwards it verbatim so the no-opt-out override lives once,
// inside the checker's implementation, rather than being re-checked at each
// of the 5 channel handlers.
type PreferenceChecker interface {
	ChannelEnabled(ctx context.Context, scope domain.Scope, subscriberID, workflowID, channel string, critical bool) (bool, error)
}

// EventPublisher publishes live events for a persisted message: a per-subscriber
// inbox event and an environment-wide delivery-status event. eventbus.Bus
// satisfies this structurally (delivery does not import eventbus). A nil
// publisher means "no live publish".
type EventPublisher interface {
	PublishInbox(environmentID, subscriberID string, data []byte) error
	PublishDelivery(environmentID string, data []byte) error
}

// Throttler gates a send by a fixed-window rate limit keyed per
// (env, subscriber, workflow). eventbus.Bus satisfies this structurally. A nil
// throttler disables throttling.
type Throttler interface {
	ThrottleAllow(key string, limit int, window time.Duration) (bool, error)
}

// Digester drains the events merged into a digest window so the channel handler
// can render an aggregate (e.g. a count). eventbus.Bus satisfies this
// structurally. A nil digester disables digest rendering: a notification with a
// DigestKey then renders per the single event like any other.
type Digester interface {
	DigestDrain(key string) ([]json.RawMessage, error)
}

// Handler persists rendered messages through the store.
type Handler struct {
	store    *store.Store
	prefs    PreferenceChecker
	pub      EventPublisher
	throttle Throttler
	digester Digester
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

// WithThrottle attaches an optional per-(env,subscriber,workflow) rate limiter
// and returns the handler for chaining. When set, a step that configures a
// throttle limit records a "throttled" message instead of sending once the
// limit is exceeded; a nil throttler disables throttling.
func (h *Handler) WithThrottle(t Throttler) *Handler {
	h.throttle = t
	return h
}

// WithDigester attaches an optional digester and returns the handler for
// chaining. When set, a self-sourced notification carrying a DigestKey drains
// its merged events and exposes {{digest.count}} to the templates; a nil
// digester or a non-digest notification renders exactly as before.
func (h *Handler) WithDigester(d Digester) *Handler {
	h.digester = d
	return h
}

// HandleInApp is registered via engine.Handle("send.in_app", h.HandleInApp).
// It unmarshals the run envelope, renders the subject/body templates against the
// {subscriber, payload} context, and persists a "sent" in-app message. On any
// unmarshal or persist error it records a "error" message (with ErrorText) and
// returns its JSON with a nil error — a failed message, not a crashed run.
func (h *Handler) HandleInApp(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var env runenv.InAppEnvelope
	var digestKey string
	var stepOutputs map[string]map[string]any
	if len(t.Input) == 0 || !isRunEnvelope(t.Input) {
		// Self-source: an empty input means an upstream step (e.g. a delay)
		// produced no output, and a non-envelope input means an upstream action
		// step (e.g. http_request) produced output that is not an InAppEnvelope.
		// Either way, rebuild the envelope from the store by run id.
		rebuilt, dk, so, err := h.rebuildEnvelope(ctx, t)
		if err != nil {
			return h.persistError(ctx, env, t, err)
		}
		env = rebuilt
		digestKey = dk
		stepOutputs = so
	} else if err := json.Unmarshal(t.Input, &env); err != nil {
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

	data := map[string]any{"subscriber": subMap, "payload": payloadMap, "steps": stepOutputs}
	// Digest aggregate: a self-sourced notification with a DigestKey drains the
	// merged events so the body can render {{digest.count}}. Only the
	// self-source path sets digestKey, so the Input-present path is unchanged.
	if digestKey != "" && h.digester != nil {
		events, _ := h.digester.DigestDrain(digestKey)
		data["digest"] = digestData(events)
	}
	translate := storeTranslator(ctx, h.store, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, subMap)
	subject := template.RenderI18n(env.Step.SubjectTemplate, data, translate)
	content := template.RenderI18n(env.Step.BodyTemplate, data, translate)
	content = wrapLayout(ctx, h.store, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, env.Step.LayoutKey, content, data, translate)

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
		return h.persistSkipped(ctx, env, t, subject, content, reason)
	}

	// Optional throttle gate. When the step configures a limit and a throttler
	// is attached, a send over the per-(env,subscriber,workflow) limit records a
	// "throttled" message instead of sending.
	if env.Step.ThrottleLimit > 0 && h.throttle != nil {
		key := env.Scope.EnvID + ":" + env.SubscriberID + ":" + env.WorkflowID
		allowed, _ := h.throttle.ThrottleAllow(key, env.Step.ThrottleLimit, time.Duration(env.Step.ThrottleWindowSec)*time.Second)
		if !allowed {
			return h.persistThrottled(ctx, env, t, subject, content)
		}
	}

	// Optional preference filtering. A nil checker always sends. On a checker
	// error we fail open (proceed to send), mirroring the reference default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, env.SubscriberID, env.WorkflowID, env.Step.Channel, env.WorkflowCritical)
		if perr == nil && !enabled {
			return h.persistSkipped(ctx, env, t, subject, content, SkipReasonPreference)
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
		Extras: inAppExtras(env.Step, func(tmpl string) string {
			return template.RenderI18n(tmpl, data, translate)
		}),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	saved, err := h.store.Messages().Create(ctx, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, msg)
	if err != nil {
		return h.persistError(ctx, env, t, err)
	}
	metrics.Delivery("in_app", metrics.OutcomeDelivered)
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

// Why a send was skipped. Recorded on the message so an operator can tell a
// misconfigured workflow from a correctly-honoured subscriber choice -- the two
// look identical as a bare "skipped" and call for opposite responses.
const (
	// SkipReasonCondition: the step's filter condition evaluated false.
	SkipReasonCondition = "condition"
	// SkipReasonConditionInvalid: the filter could not be evaluated at all. The
	// gate fails CLOSED, so a broken condition skips rather than sending to
	// everyone -- which makes it indistinguishable from an honest false without
	// this distinction, and it is the case most likely to be a real bug.
	SkipReasonConditionInvalid = "condition_invalid"
	// SkipReasonPreference: the subscriber has this channel disabled. Not a
	// fault; the system did exactly what was asked.
	SkipReasonPreference = "preference"
)

// persistSkipped records a status:"skipped" in-app message (the channel is
// disabled by subscriber preference) and returns its JSON with a nil error.
// No send occurs; this mirrors the existing skipped/failed-message pattern.
func (h *Handler) persistSkipped(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, subject, content, reason string) (json.RawMessage, error) {
	metrics.Delivery("in_app", metrics.OutcomeSkipped)
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
		SkipReason:     reason,
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

// persistThrottled records a status:"throttled" in-app message (the send
// exceeded the step's per-(env,subscriber,workflow) rate limit) and returns its
// JSON with a nil error. No send occurs; mirrors the persistSkipped pattern.
func (h *Handler) persistThrottled(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, subject, content string) (json.RawMessage, error) {
	metrics.Delivery("in_app", metrics.OutcomeThrottled)
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

// persistError records a status:"error" message carrying cause and returns its
// JSON with a nil error. Best-effort: if persistence itself fails, the in-memory
// message is still marshalled and returned so the caller observes the failure.
func (h *Handler) persistError(ctx context.Context, env runenv.InAppEnvelope, t engine.TaskPayload, cause error) (json.RawMessage, error) {
	metrics.Delivery("in_app", metrics.OutcomeError)
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

// inAppExtras snapshots the in_app rich fields authored on the step into the
// message extras blob, using the reference platform's inAppOutputSchema field names so the inbox
// API output is wire-compatible. Absent fields are omitted; a step with no rich
// fields yields "{}". Only the sent path snapshots extras — skipped/throttled/
// errored messages leave Extras nil and the repo coerces it to "{}".
//
// render evaluates one authored string against the run payload. The rich
// fields are authored in the same editor and the same {{path}} namespace as
// subject and body, so they are rendered the same way: a redirect stored
// verbatim reaches the inbox API as the literal "{{payload.link}}" and every
// row linking to it is dead (#328). Callers pass a closure over the data and
// translate func they already built for subject/body; a nil render renders
// nothing, which keeps the function usable from tests that do not care.
func inAppExtras(step runenv.Step, render func(string) string) json.RawMessage {
	if render == nil {
		render = func(s string) string { return s }
	}
	payload := struct {
		Avatar          string                `json:"avatar,omitempty"`
		PrimaryAction   *runenv.InAppAction   `json:"primaryAction,omitempty"`
		SecondaryAction *runenv.InAppAction   `json:"secondaryAction,omitempty"`
		Redirect        *runenv.InAppRedirect `json:"redirect,omitempty"`
		Data            json.RawMessage       `json:"data,omitempty"`
	}{
		Avatar:          render(step.Avatar),
		PrimaryAction:   renderInAppAction(step.PrimaryAction, render),
		SecondaryAction: renderInAppAction(step.SecondaryAction, render),
		Redirect:        renderInAppRedirect(step.Redirect, render),
		Data:            renderJSONStrings(step.InAppData, render),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Reachable only via a hand-poked InAppData blob (every other field is a
		// typed struct that always marshals), but the consequence is invisible:
		// the message still sends, just stripped of its avatar, actions and
		// redirect. Without this the recipient sees a plain notification and
		// nothing anywhere records that the rich fields were dropped.
		slog.Default().Warn(
			"delivery: in_app extras dropped, sending without rich fields",
			"err", err,
			"hasAvatar", step.Avatar != "",
			"hasPrimaryAction", step.PrimaryAction != nil,
			"hasSecondaryAction", step.SecondaryAction != nil,
			"hasRedirect", step.Redirect != nil,
			"dataBytes", len(step.InAppData),
		)
		return json.RawMessage("{}")
	}
	return b
}

// renderInAppRedirect returns a NEW redirect with its url rendered, or nil when
// the url renders empty — mirroring inAppRedirectFromForm, which omits the
// whole object for a blank url even when target is set. Target is not a
// template: it is _blank/_self, not authorable content.
//
// A new value rather than a mutated one: Redirect is a pointer into the run
// envelope, and rendering in place would overwrite the authored template with
// one subscriber's payload for every step that reads it afterwards.
func renderInAppRedirect(r *runenv.InAppRedirect, render func(string) string) *runenv.InAppRedirect {
	if r == nil {
		return nil
	}
	url := render(r.URL)
	if url == "" {
		return nil
	}
	return &runenv.InAppRedirect{URL: url, Target: r.Target}
}

// renderInAppAction returns a NEW action with its label and redirect rendered,
// or nil when the label renders empty — mirroring inAppActionFromForm. The
// action's redirect drops independently, on its own blank-url rule.
func renderInAppAction(a *runenv.InAppAction, render func(string) string) *runenv.InAppAction {
	if a == nil {
		return nil
	}
	label := render(a.Label)
	if label == "" {
		return nil
	}
	return &runenv.InAppAction{Label: label, Redirect: renderInAppRedirect(a.Redirect, render)}
}

// renderJSONStrings renders every string leaf of a free-form JSON value,
// walking objects and arrays. Object KEYS are left alone: they are the
// consumer's contract for reading the blob, not authored copy.
//
// in_app "data" is authored as arbitrary JSON (steptype.parseDataObject only
// requires an object), so there is no fixed set of fields to render one by one.
// Unparseable input is passed through untouched, leaving the caller's existing
// marshal-failure path (warn, send without rich fields) to handle it exactly as
// it did before rendering existed.
func renderJSONStrings(raw json.RawMessage, render func(string) string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(renderJSONValue(v, render))
	if err != nil {
		return raw
	}
	return out
}

// renderJSONValue is renderJSONStrings's recursion over a decoded JSON value.
func renderJSONValue(v any, render func(string) string) any {
	switch t := v.(type) {
	case string:
		return render(t)
	case map[string]any:
		for k, val := range t {
			t[k] = renderJSONValue(val, render)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = renderJSONValue(val, render)
		}
		return t
	default:
		return v
	}
}
