package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/metrics"
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
//
// ResolveEmailIntegration resolves the provider, From address, providerID,
// and integrationID for a specific integration identifier within (scope,
// "email"), bypassing the primary-integration resolution.
// connectivity.Service.ResolveEmailIntegration satisfies this — its return
// shape matches ResolveEmail's exactly, so an integration-routed send picks
// up the integration's own configured From just like the primary path does.
type EmailResolver interface {
	ResolveEmail(ctx context.Context, scope domain.Scope) (providers.EmailProvider, string, string, string, error)
	ResolveEmailIntegration(ctx context.Context, scope domain.Scope, identifier string) (providers.EmailProvider, string, string, string, error)
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
	armer    ReplyArmer
}

// ReplyArmer arms a reply-to-notification thread for one recipient before an
// email step sends, returning the tokenized reply mailbox to put on the
// message. ok=false with no error means the loop cannot take a reply for this
// scope (inbound not enabled, no verified inbound domain): the send proceeds
// with the step's static Reply-To rather than promising a reply it cannot
// honour. emailloop.Service satisfies this.
type ReplyArmer interface {
	ArmReply(ctx context.Context, scope domain.Scope, notificationID, transactionID, workflowIdentifier, subscriberID, subscriberEmail string) (replyTo string, ok bool, err error)
	// DisarmReply drops the thread ArmReply bound when the send failed, so
	// no mailbox stays live for a message that never left.
	DisarmReply(ctx context.Context, scope domain.Scope, notificationID string) error
}

// WithReplyArmer attaches the reply loop. Without it a step's Reply opt-in is
// logged and ignored rather than failing the send.
func (h *EmailHandler) WithReplyArmer(a ReplyArmer) *EmailHandler {
	h.armer = a
	return h
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

// resolveEmailIDs resolves the providerID and integrationID strings only,
// honoring an explicit step-level integration identifier when set. Used on
// pre-send paths (skipped, throttled) where we want the IDs for attribution
// but the provider itself is not needed. A resolve failure returns empty
// strings; callers treat "" as "unknown" and continue rather than blocking.
func (h *EmailHandler) resolveEmailIDs(ctx context.Context, scope domain.Scope, identifier string) (providerID, integrationID string) {
	if identifier != "" {
		_, _, pid, iid, err := h.resolver.ResolveEmailIntegration(ctx, scope, identifier)
		if err != nil {
			return "", ""
		}
		return pid, iid
	}
	_, _, pid, iid, err := h.resolver.ResolveEmail(ctx, scope)
	if err != nil {
		return "", ""
	}
	return pid, iid
}

// resolveEmailProvider resolves the EmailProvider, from address, providerID,
// and integrationID for the send, routing through ResolveEmailIntegration
// when the step sets an explicit "integration" identifier and through the
// primary-resolution path (ResolveEmail) otherwise — byte-identical to
// pre-#314 behavior when unset. Both paths return the integration's
// configured From address; the caller's existing env.Step.From override
// still applies on top of either.
func (h *EmailHandler) resolveEmailProvider(ctx context.Context, scope domain.Scope, identifier string) (providers.EmailProvider, string, string, string, error) {
	if identifier == "" {
		return h.resolver.ResolveEmail(ctx, scope)
	}
	return h.resolver.ResolveEmailIntegration(ctx, scope, identifier)
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
	var stepOutputs map[string]map[string]any
	if len(t.Input) == 0 || !isRunEnvelope(t.Input) {
		// Self-source: an empty input means an upstream step (e.g. a delay)
		// produced no output; a non-envelope input means an upstream action step
		// (e.g. http_request) produced non-envelope output. Either way, rebuild
		// the envelope from the store by run id.
		rebuilt, dk, so, err := RebuildEnvelope(ctx, h.store, t)
		if err != nil {
			return h.persistEmailError(ctx, env, t, "", "", err)
		}
		env = rebuilt
		digestKey = dk
		stepOutputs = so
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

	data := map[string]any{"subscriber": subMap, "payload": payloadMap, "steps": stepOutputs}
	// Digest aggregate: a self-sourced notification with a DigestKey drains the
	// merged events so the body can render {{digest.count}}. Only the
	// self-source path sets digestKey, so the Input-present path is unchanged.
	if digestKey != "" && h.digester != nil {
		events, _ := h.digester.DigestDrain(digestKey)
		data["digest"] = digestData(events)
	}
	translate := storeTranslator(ctx, h.store, domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, subMap)
	// A subject is never HTML, and neither is a text body: values go in as
	// sent. Only an HTML body escapes them (#364). A step with an HTML body
	// alongside (#369) sends both: Body is then always the text part, and the
	// layout, which is HTML, wraps only the HTML part.
	multipart := env.Step.HTMLBodyTemplate != ""
	bodyEsc := template.Plain
	if env.Step.EditorType == "html" && !multipart {
		bodyEsc = template.HTML
	}
	subject := template.RenderAs(template.Plain, env.Step.SubjectTemplate, data, translate)
	content := template.RenderAs(bodyEsc, env.Step.BodyTemplate, data, translate)
	to, _ := subMap["email"].(string)

	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
	var htmlContent string
	if multipart {
		htmlContent = template.RenderAs(template.HTML, env.Step.HTMLBodyTemplate, data, translate)
		htmlContent = wrapLayout(ctx, h.store, scope, env.Step.LayoutKey, htmlContent, data, translate, template.HTML)
	} else {
		content = wrapLayout(ctx, h.store, scope, env.Step.LayoutKey, content, data, translate, bodyEsc)
	}

	// Per-step condition gate. When the step authored a filter condition, it is
	// evaluated against the {subscriber, payload} context; a false (or
	// unevaluable) condition SKIPS the send. Absent/empty filters always run. A
	// malformed condition fails closed (skip) so a broken condition never blasts
	// a send to every subscriber.
	if !conditionMet(env.Step.Filters, subMap, payloadMap, stepOutputs) {
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
	// error we fail open (proceed to send), a default-enabled
	// behaviour rather than dropping the notification on a transient lookup fault.
	if h.prefs != nil {
		enabled, perr := h.prefs.ChannelEnabled(ctx, scope, env.SubscriberID, env.WorkflowID, env.Step.Channel, env.WorkflowCritical)
		if perr == nil && !enabled {
			return h.persistEmailSkipped(ctx, env, t, scope, subject, content)
		}
	}
	provider, from, providerID, integrationID, err := h.resolveEmailProvider(ctx, scope, env.Step.Integration)
	if err != nil {
		return h.persistEmailError(ctx, env, t, "", "", err)
	}

	// Per-step From override: step-level address wins over the integration default.
	effectiveFrom := from
	if env.Step.From != "" {
		effectiveFrom = env.Step.From
	}

	// With an HTML body alongside, both fields are set and the provider sends
	// multipart/alternative. Otherwise EditorType selects the one field:
	// "html" → HTML body, "" or "block" → Text body.
	var msgHTML, msgText string
	switch {
	case multipart:
		msgText, msgHTML = content, htmlContent
	case env.Step.EditorType == "html":
		msgHTML = content
	default:
		msgText = content
	}

	// Reply-to-notification (#345): arm the thread BEFORE the send, so a reply
	// can never arrive at a mailbox nothing is bound to -- the same
	// bind-before-dispatch rule the chat and conversation loops follow. An arm
	// failure fails the send: a message that promises a reply it cannot take
	// is worse than no message.
	replyTo := env.Step.ReplyTo
	armed := false
	if env.Step.ReplyEnabled && h.armer == nil {
		slog.Default().Warn("delivery: email step asks for replies but no reply loop is wired; sending without",
			"notification", env.NotificationID, "workflow", env.WorkflowIdentifier)
	}
	if env.Step.ReplyEnabled && h.armer != nil {
		mailbox, ok, aerr := h.armer.ArmReply(ctx, scope, env.NotificationID, env.TransactionID,
			env.WorkflowIdentifier, env.SubscriberID, to)
		if aerr != nil {
			return h.persistEmailError(ctx, env, t, providerID, integrationID, fmt.Errorf("arm reply thread: %w", aerr))
		}
		if ok {
			replyTo, armed = mailbox, true
		} else {
			slog.Default().Info("delivery: email step asks for replies but inbound is not available for this scope; sending without",
				"notification", env.NotificationID, "workflow", env.WorkflowIdentifier)
		}
	}

	if _, err := provider.Send(ctx, providers.EmailMessage{
		From:    effectiveFrom,
		To:      to,
		Subject: subject,
		Text:    msgText,
		HTML:    msgHTML,
		ReplyTo: replyTo,
	}); err != nil {
		if armed {
			if derr := h.armer.DisarmReply(ctx, scope, env.NotificationID); derr != nil {
				slog.Default().Error("delivery: reply thread left armed for a send that failed",
					"notification", env.NotificationID, "err", derr)
			}
		}
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
	metrics.Delivery("email", metrics.OutcomeDelivered)
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
	metrics.Delivery("email", metrics.OutcomeSkipped)
	providerID, integrationID := h.resolveEmailIDs(ctx, scope, env.Step.Integration)
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
	metrics.Delivery("email", metrics.OutcomeThrottled)
	providerID, integrationID := h.resolveEmailIDs(ctx, scope, env.Step.Integration)
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
	metrics.Delivery("email", metrics.OutcomeError)
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
