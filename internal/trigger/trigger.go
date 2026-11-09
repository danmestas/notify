// Package trigger implements POST /v1/events/trigger: it upserts the recipient,
// resolves the named workflow's first step, records the notification, builds
// the runenv.InAppEnvelope (the StartRun input the delivery worker consumes)
// and kicks off an engine run. It is the producer side of the wire contracts
// pinned in internal/runenv.
package trigger

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/keypath"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/observe"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// Service orchestrates a trigger: audience upsert -> workflow lookup ->
// notification persist -> envelope build -> engine StartRun.
type Service struct {
	aud      *audience.Service
	s        *store.Store
	eng      RunStarter
	digester Digester
	windows  DigestWindowResolver
}

// DigestWindowResolver reports a subscriber's per-workflow digest window
// override. preferences.Service satisfies it. A nil resolver means "no
// overrides": every subscriber gets the step's authored window, which is the
// behaviour before #336.
type DigestWindowResolver interface {
	DigestWindowFor(ctx context.Context, scope domain.Scope, subscriberID, workflowID string) (domain.DigestWindow, bool, error)
}

// Digester batches rapid trigger events for the same (env, subscriber,
// workflow) into one downstream run. eventbus.Bus satisfies this structurally.
// A nil digester disables batching: a digest-first workflow then sends per
// event instead of merging.
type Digester interface {
	DigestAppend(key string, event json.RawMessage) error
	DigestClaim(key string) (bool, error)
}

// RunStarter starts engine runs. *engine.Engine satisfies it; its errors are
// read with engine.StartRefused.
type RunStarter interface {
	StartRun(ctx context.Context, workflow string, input json.RawMessage) (string, error)
}

// New builds a trigger Service over its collaborators.
func New(aud *audience.Service, s *store.Store, eng RunStarter) *Service {
	return &Service{aud: aud, s: s, eng: eng}
}

// WithDigester attaches an optional digester and returns the Service for
// chaining. When set, a workflow whose first step is "digest" merges rapid
// events into one run; a nil digester leaves trigger behaviour unchanged.
// WithDigestWindows attaches the resolver consulted for per-subscriber digest
// cadences. Optional: without it every subscriber uses the step's window.
func (svc *Service) WithDigestWindows(r DigestWindowResolver) *Service {
	svc.windows = r
	return svc
}

func (svc *Service) WithDigester(d Digester) *Service {
	svc.digester = d
	return svc
}

// ToField is the trigger recipient. A single-subscriber trigger sets
// SubscriberID (plus optional profile fields); a multicast trigger sets
// TopicKey instead, fanning out to every member of that topic.
type ToField struct {
	SubscriberID string `json:"subscriberId"`
	Email        string `json:"email"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Phone        string `json:"phone"`
	TopicKey     string `json:"topicKey,omitempty"`
}

// TriggerRequest is the POST /v1/events/trigger body.
type TriggerRequest struct {
	Name          string          `json:"name"`
	To            ToField         `json:"to"`
	Payload       json.RawMessage `json:"payload"`
	TransactionID string          `json:"transactionId"`
}

// TriggerResponse is the trigger-event response DTO:
// Error and activityFeedLink are omitted on success; TransactionID echoes the
// client value or the server-minted id.
type TriggerResponse struct {
	Acknowledged  bool     `json:"acknowledged"`
	Status        string   `json:"status"`
	TransactionID string   `json:"transactionId"`
	Error         []string `json:"error,omitempty"`
}

// wireSubscriber is the handlebars context object for {{subscriber.*}} — camelCase
// namespace keys, NOT Go field names, so templates resolve. It is marshaled
// into the envelope's Subscriber raw JSON.
type wireSubscriber struct {
	SubscriberID string          `json:"subscriberId"`
	FirstName    string          `json:"firstName"`
	LastName     string          `json:"lastName"`
	Email        string          `json:"email"`
	Phone        string          `json:"phone"`
	Locale       string          `json:"locale"`
	Timezone     string          `json:"timezone"`
	Data         json.RawMessage `json:"data,omitempty"`
}

// digestKeyPath extracts the optional digestKey dot-path from a digest step's
// controlValues (e.g. "payload.orderId"). An absent/empty field means "group by
// the per-subscriber base key", preserving the original behaviour.
func digestKeyPath(controlValues json.RawMessage) string {
	if len(controlValues) == 0 {
		return ""
	}
	var cv struct {
		DigestKey string `json:"digestKey"`
	}
	if err := json.Unmarshal(controlValues, &cv); err != nil {
		return ""
	}
	return cv.DigestKey
}

// resolveDigestKeyValue resolves a digestKey dot-path against the event.
// Shared with throttle's throttleKey; see internal/keypath.
func resolveDigestKeyValue(path string, payload, subscriber json.RawMessage) string {
	return keypath.Resolve(path, payload, subscriber)
}

// buildDigestKey appends the resolved grouping value to the base window key. An
// empty value returns the bare base (so absent-field events merge into the
// per-subscriber bucket). A non-empty value is base64url-encoded before being
// appended so the NATS-KV key sanitizer (which collapses ':' and '_' alike to
// '_') cannot silently merge two distinct values such as "order:001" and
// "order_001" — the encoded segment keeps the mapping injective.
func buildDigestKey(base, value string) string {
	if value == "" {
		return base
	}
	return base + ":" + base64.RawURLEncoding.EncodeToString([]byte(value))
}

// Ingest processes one trigger end-to-end. On an unknown workflow it returns a
// non-error response with Acknowledged=false / Status="error" (documented
// choice: trigger ingestion is best-effort acknowledgement, with the
// response DTO carrying an error array rather than a transport error).
// An inactive workflow is silently skipped: Acknowledged=false, Status="error",
// no engine run, no notification row. This matches the activate-toggle contract
// (spec-02 B4): deactivating a workflow causes triggers to no-op cleanly.
func (svc *Service) Ingest(ctx context.Context, scope domain.Scope, req TriggerRequest) (resp TriggerResponse, err error) {
	// Root span for the whole trigger -> StartRun -> delivery path.
	ctx, span := observe.Tracer().Start(
		ctx, "notify.trigger.ingest",
		trace.WithAttributes(
			attribute.String("workflow.name", req.Name),
			attribute.String("env.id", scope.EnvID),
		),
	)
	// Named-return defer rather than per-return calls: Ingest has many error
	// exits (store lookups, fan-out, per-subscriber ingest) and this marks all
	// of them — including ones added later — so failed triggers are findable by
	// span status, matching engine.request.
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	wf, err := svc.s.Workflows().GetByTriggerIdentifier(ctx, scope, req.Name)
	if errors.Is(err, store.ErrNotFound) {
		return TriggerResponse{
			Acknowledged: false,
			Status:       "error",
			Error:        []string{"workflow not found"},
		}, nil
	}
	if err != nil {
		return TriggerResponse{}, err
	}

	// Inactive workflows must no-op at ingest time: no run is started and no
	// notification row is written. The acknowledged=false / Status="error"
	// shape matches the existing unknown-workflow response so callers get a
	// consistent non-success signal without a transport error.
	if !wf.Active {
		return TriggerResponse{
			Acknowledged: false,
			Status:       "error",
			Error:        []string{"workflow is inactive"},
		}, nil
	}

	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return TriggerResponse{}, err
		}
	}
	if len(steps) == 0 {
		return TriggerResponse{
			Acknowledged: false,
			Status:       "error",
			Error:        []string{"workflow has no steps"},
		}, nil
	}

	// transactionID is minted once and shared by every fan-out run so a
	// multicast emits a single logical transaction across all recipients.
	transactionID := req.TransactionID
	if transactionID == "" {
		transactionID = store.NewULID()
	}
	req.TransactionID = transactionID

	// Multicast: fan out to every member of the topic. The subscriber profiles
	// already exist (added to the topic out of band), so no per-recipient
	// upsert happens here; each member's persisted profile is resolved inside
	// ingestForSubscriber for template rendering.
	if req.To.TopicKey != "" {
		if _, err := svc.s.Topics().GetByKey(ctx, scope, req.To.TopicKey); errors.Is(err, store.ErrNotFound) {
			return TriggerResponse{
				Acknowledged: false,
				Status:       "error",
				Error:        []string{"topic not found"},
			}, nil
		} else if err != nil {
			return TriggerResponse{}, err
		}
		subs, err := svc.s.Topics().ListSubscriberIDs(ctx, scope, req.To.TopicKey)
		if err != nil {
			return TriggerResponse{}, err
		}
		// Record the (transaction_id, topic_key) association before fanning out
		// so the Topic Activity Feed can join analytics_deliveries via
		// transaction_id even when the fan-out has zero members.
		if err := svc.s.TopicTriggers().Record(ctx, scope, transactionID, req.To.TopicKey); err != nil {
			return TriggerResponse{}, err
		}
		for _, subscriberID := range subs {
			if _, err := svc.ingestForSubscriber(ctx, scope, wf, subscriberID, req); err != nil {
				return TriggerResponse{}, err
			}
		}
		return TriggerResponse{
			Acknowledged:  true,
			Status:        "processed",
			TransactionID: transactionID,
		}, nil
	}

	// Single-subscriber: upsert the to-field profile, then run for that one
	// recipient. Behaviour here is unchanged from the original Ingest.
	if _, err := svc.aud.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: req.To.SubscriberID,
		Email:        req.To.Email,
		FirstName:    req.To.FirstName,
		LastName:     req.To.LastName,
		Phone:        req.To.Phone,
	}); err != nil {
		return TriggerResponse{}, err
	}
	if _, err := svc.ingestForSubscriber(ctx, scope, wf, req.To.SubscriberID, req); err != nil {
		return TriggerResponse{}, err
	}
	return TriggerResponse{
		Acknowledged:  true,
		Status:        "processed",
		TransactionID: transactionID,
	}, nil
}

// ingestForSubscriber performs the per-recipient work: resolve the persisted
// subscriber profile, honour digest coordination, record the notification,
// build the envelope from the workflow's first step and start the engine run.
// It assumes req.TransactionID is already minted and wf has at least one step.
// A digest-merged event returns an empty runID with no error (no run started).
func (svc *Service) ingestForSubscriber(ctx context.Context, scope domain.Scope, wf domain.Workflow, subscriberID string, req TriggerRequest) (string, error) {
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		return "", err
	}
	step := steps[0]
	var controls runenv.StepControls
	if len(step.ControlValues) > 0 {
		if err := json.Unmarshal(step.ControlValues, &controls); err != nil {
			return "", err
		}
	}

	sub, err := svc.aud.Get(ctx, scope, subscriberID)
	if err != nil {
		return "", err
	}
	subscriberJSON, err := json.Marshal(wireSubscriber{
		SubscriberID: sub.SubscriberID,
		FirstName:    sub.FirstName,
		LastName:     sub.LastName,
		Email:        sub.Email,
		Phone:        sub.Phone,
		Locale:       sub.Locale,
		Timezone:     sub.Timezone,
		Data:         sub.Data,
	})
	if err != nil {
		return "", err
	}

	// Digest coordination: when the workflow's first step is "digest" and a
	// digester is attached, append this event to the digest window and claim
	// ownership. The window key is the (env, subscriber, workflow) base,
	// optionally extended by a configured digestKey value resolved from the
	// event payload or subscriber profile (so e.g. orders with distinct orderId
	// values batch into separate digests). Exactly one trigger wins the claim
	// and starts the run; the rest merge silently (no new run) and are drained
	// + rendered by the owning run after the window fires.
	var digestKey string
	var digestDeadline string
	if step.Type == "digest" && svc.digester != nil {
		base := scope.EnvID + ":" + subscriberID + ":" + wf.ID
		path := digestKeyPath(step.ControlValues)
		value := resolveDigestKeyValue(path, req.Payload, subscriberJSON)
		if path != "" && value == "" {
			// A configured key that resolves to nothing silently collapses
			// every event into one bucket. Authoring now refuses an unrooted
			// path, but steps stored before that still carry one, and a rooted
			// path can also miss on a payload that lacks the field -- either
			// way the author gets a digest that merges unrelated events and no
			// other signal that it happened (#336).
			slog.Default().Warn(
				"trigger: digestKey resolved to nothing, events will merge into one digest",
				"digest_key_path", path, "workflow_id", wf.ID,
				"trigger_identifier", wf.TriggerIdentifier, "subscriber_id", subscriberID,
			)
		}
		digestKey = buildDigestKey(base, value)
		// Resolve THIS subscriber's window and carry the closing instant in the
		// envelope; the compiled sleep reads it back by dot-path at dispatch.
		var wErr error
		digestDeadline, wErr = svc.resolveDigestDeadline(ctx, scope, subscriberID, wf.ID, step.ControlValues, time.Now().UTC())
		if wErr != nil {
			return "", wErr
		}
		if err := svc.digester.DigestAppend(digestKey, req.Payload); err != nil {
			return "", err
		}
		isOwner, _ := svc.digester.DigestClaim(digestKey)
		if !isOwner {
			return "", nil
		}
	}
	var stepData map[string]string
	if controls.DataJSON != "" {
		if err := json.Unmarshal([]byte(controls.DataJSON), &stepData); err != nil {
			return "", fmt.Errorf("trigger: unmarshal data_json for step %s: %w", step.StepID, err)
		}
	}
	notif, err := svc.notificationFor(ctx, scope, wf.ID, subscriberID, req, digestKey)
	if err != nil {
		return "", err
	}
	if notif.RunID != "" {
		// An earlier attempt of this same trigger started the run and recorded
		// it; a retry starts nothing.
		return notif.RunID, nil
	}

	envelope := runenv.InAppEnvelope{
		Scope:              runenv.Scope{OrgID: scope.OrgID, EnvID: scope.EnvID},
		TransactionID:      req.TransactionID,
		WorkflowID:         wf.ID,
		WorkflowCritical:   wf.Critical,
		WorkflowIdentifier: wf.TriggerIdentifier,
		NotificationID:     notif.ID,
		SubscriberID:       subscriberID,
		Subscriber:         subscriberJSON,
		Payload:            req.Payload,
		// IdempotencyKey composes TransactionID with SubscriberID (base64url
		// so an embedded ':' can't blur the boundary between the two parts —
		// same technique as buildDigestKey above). A multicast trigger shares
		// one TransactionID across every topic member, so TransactionID alone
		// would collide their runs; per-recipient composition dedupes only a
		// genuine retry of the same (transaction, recipient) pair.
		IdempotencyKey: req.TransactionID + ":" + base64.RawURLEncoding.EncodeToString([]byte(subscriberID)),
		DigestDeadline: digestDeadline,
		Delays:         dynamicDelays(steps, req.Payload, wf),
		Step: runenv.Step{
			StepID:            step.StepID,
			Channel:           step.Type,
			ProviderID:        "",
			SubjectTemplate:   controls.SubjectTemplate(),
			BodyTemplate:      controls.Body,
			HTMLBodyTemplate:  controls.HTMLBody,
			Topic:             controls.Topic,
			ThrottleLimit:     controls.Throttle.Limit,
			ThrottleWindowSec: controls.Throttle.WindowSec,
			LayoutKey:         controls.LayoutKey,
			Priority:          controls.Priority,
			Tags:              controls.Tags,
			Markdown:          controls.Markdown,
			Data:              stepData,
			From:              controls.From,
			ReplyTo:           controls.ReplyTo,
			ReplyEnabled:      controls.Reply,
			EditorType:        controls.EditorType,
			Avatar:            controls.Avatar,
			PrimaryAction:     controls.PrimaryAction,
			SecondaryAction:   controls.SecondaryAction,
			Redirect:          controls.Redirect,
			InAppData:         controls.Data,
			Integration:       controls.Integration,
			DefaultEnabled:    controls.DefaultEnabled,
			Filters:           step.Filters,
		},
	}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}

	// DAGName = scope.EnvID + "-" + triggerIdentifier. dagnats maps the
	// workflow name onto a NATS KV key, which rejects ":" ("nats: invalid
	// key"). "-" is used rather than "." so the name stays a single NATS
	// subject token; it is proven end-to-end by the engine smoke workflow.
	// authoring registers under the identical name (authoring.DAGName).
	dagName := names.Workflow(scope.EnvID, req.Name)
	return svc.start(ctx, scope, notif.ID, dagName, envelopeJSON)
}

// notificationFor returns the notification row this trigger delivers under,
// writing it as pending if no earlier attempt of the same trigger (same
// workflow, subscriber and transaction) left one. The row is written before
// the run starts so that the run's steps always find it (#66); reusing it on a
// retry keeps one row per trigger, and the run's idempotency key (the
// envelope's IdempotencyKey, the same transaction and subscriber) keeps one run.
func (svc *Service) notificationFor(ctx context.Context, scope domain.Scope, workflowID, subscriberID string, req TriggerRequest, digestKey string) (domain.Notification, error) {
	n, err := svc.s.Notifications().GetByTrigger(ctx, scope, workflowID, subscriberID, req.TransactionID)
	if !errors.Is(err, store.ErrNotFound) {
		return n, err
	}
	return svc.s.Notifications().Create(ctx, scope, domain.Notification{
		ID:            store.NewULID(),
		WorkflowID:    workflowID,
		SubscriberID:  subscriberID,
		TransactionID: req.TransactionID,
		Payload:       req.Payload,
		DigestKey:     digestKey,
		CreatedAt:     timestamp.Now(),
		Status:        domain.NotificationPending,
	})
}

// start starts the run for notification notificationID and records its run id.
//
// A start the engine refused leaves no run, so the row is marked failed. Any
// other error (a timeout, a cancelled request) leaves it unknown whether
// dagnats started the run, so the row stays pending: the run's first step
// records its run id if it did start, and a retried trigger reuses the row and
// asks again, which dagnats answers with the same run.
//
// The writes after StartRun outlive a cancelled request: the run exists or
// was refused whether or not the caller is still waiting.
func (svc *Service) start(ctx context.Context, scope domain.Scope, notificationID, dagName string, envelope json.RawMessage) (string, error) {
	runID, err := svc.eng.StartRun(ctx, dagName, envelope)
	wctx := context.WithoutCancel(ctx)
	if err != nil {
		if engine.StartRefused(err) {
			if mErr := svc.s.Notifications().MarkFailed(wctx, scope, notificationID); mErr != nil {
				return "", errors.Join(err, fmt.Errorf("trigger: mark notification %s failed: %w", notificationID, mErr))
			}
		}
		return "", err
	}
	// false means a step of the run recorded the id first; the row is right
	// either way.
	if _, err := svc.s.Notifications().AttachRun(wctx, scope, notificationID, runID); err != nil {
		return "", fmt.Errorf("trigger: record run %s on notification %s: %w", runID, notificationID, err)
	}
	return runID, nil
}

// dynamicDelays resolves every dynamic delay step that compiles to a native
// sleep into the value that sleep reads at delays.<stepID>. A payload value
// that does not match the step's declared kind is left out and logged: the
// sleep then fails the run at that step, as the delay worker did before (#258).
func dynamicDelays(steps []domain.Step, payload json.RawMessage, wf domain.Workflow) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	for _, step := range steps {
		key, kind, ok := steptype.NativeDynamicDelay(step)
		if !ok {
			continue
		}
		v, err := steptype.DynamicDelayInput(payload, key, kind)
		if err != nil {
			slog.Default().Warn("trigger: dynamic delay value unusable, the run will fail at this step",
				"step_id", step.StepID, "workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier, "err", err)
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage)
		}
		out[step.StepID] = v
	}
	return out
}

// resolveDigestDeadline returns the RFC3339 instant a regular digest window
// closes for one subscriber: now plus the subscriber's override, or the step's
// authored window when there is none.
//
// A scheduled (cron) digest returns "" -- it compiles to a digest.wait task
// that resolves its own cron at dispatch and never reads the deadline.
//
// A resolver error is returned rather than swallowed: falling back to the step
// window on a failed preference read would quietly ignore a cadence the
// subscriber has actually set, which is the class of silent-wrong-answer bug
// this file has been bitten by before.
func (svc *Service) resolveDigestDeadline(
	ctx context.Context, scope domain.Scope, subscriberID, workflowID string,
	controlValues json.RawMessage, now time.Time,
) (string, error) {
	authored, scheduled, err := steptype.DigestWindow(controlValues)
	if err != nil {
		return "", fmt.Errorf("trigger: digest window for workflow %s: %w", workflowID, err)
	}
	if scheduled {
		return "", nil
	}
	window := authored
	if svc.windows != nil {
		override, ok, oErr := svc.windows.DigestWindowFor(ctx, scope, subscriberID, workflowID)
		if oErr != nil {
			return "", fmt.Errorf("trigger: resolve digest window override: %w", oErr)
		}
		if ok {
			window = override
		}
	}
	unit, err := steptype.UnitDuration(window.Unit)
	if err != nil {
		return "", fmt.Errorf("trigger: digest window unit: %w", err)
	}
	return now.Add(time.Duration(window.Amount) * unit).Format(time.RFC3339), nil
}
