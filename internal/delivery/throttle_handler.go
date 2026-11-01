package delivery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/keypath"
	"github.com/danmestas/notify/internal/metrics"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// TaskThrottle is the engine task name that the throttle step type's Compile
// emits. cmd/notify/main.go must register an eng.Handle for this name; if the
// handler is absent, the engine permanently fails any run that contains a
// throttle step with a matching error, making the omission immediately visible.
const TaskThrottle = names.CheckThrottle

// ThrottleHandler implements the check.throttle engine task. It gates an entire
// run by a fixed-window counter keyed per (envID, subscriberID, workflowID).
// Under the configured limit it returns (nil, nil) so downstream steps proceed;
// at or over the limit it persists a status:"throttled" message row (so the
// suppression is visible in the Activity view, mirroring the inline send-step
// throttle handlers) and returns a NonRetryableError that permanently fails
// the run — no channel steps execute. check.throttle compiles to a plain
// sequential "normal" dagnats task like every send step (steptype.throttle.
// Compile), so returning (nil, nil) here would let downstream steps run; the
// NonRetryableError is the only mechanism that stops them, so it stays even
// though the failure is not a delivery fault.
type ThrottleHandler struct {
	store    *store.Store
	throttle Throttler
}

// NewThrottleHandler constructs a ThrottleHandler backed by s and using t as
// the fixed-window counter. eventbus.Bus satisfies Throttler structurally.
func NewThrottleHandler(s *store.Store, t Throttler) *ThrottleHandler {
	return &ThrottleHandler{store: s, throttle: t}
}

// Handle implements engine.HandlerFunc for check.throttle.
//
// Branch pattern (matches every channel handler):
//
//	if len(t.Input) != 0: throttle is the first step; parse the trigger
//	envelope for scope/subscriberID/workflowID (valid even though
//	ThrottleLimit in the envelope is 0 for standalone throttle steps), then
//	call GetByID once to load step ControlValues. This path avoids the
//	GetByRunID race that would occur if the notification row is not yet
//	committed when the engine dispatches the first task.
//
//	else: throttle follows a sleep/delay step whose nil output left Input
//	empty; rebuild identifiers from the notification row via GetByRunID.
func (h *ThrottleHandler) Handle(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var (
		scope          domain.Scope
		workflowID     string
		subscriberID   string
		payload        json.RawMessage
		notificationID string
		transactionID  string
	)

	if len(t.Input) != 0 {
		var env runenv.InAppEnvelope
		if err := json.Unmarshal(t.Input, &env); err != nil {
			return nil, fmt.Errorf("throttle: unmarshal input: %w", err)
		}
		scope = domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
		workflowID = env.WorkflowID
		subscriberID = env.SubscriberID
		payload = env.Payload
		notificationID = env.NotificationID
		transactionID = env.TransactionID
	} else {
		notif, err := NotificationByRun(ctx, h.store, t.RunID)
		if err != nil {
			return nil, fmt.Errorf("throttle: notification by run %q: %w", t.RunID, err)
		}
		scope = domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}
		workflowID = notif.WorkflowID
		subscriberID = notif.SubscriberID
		payload = notif.Payload
		notificationID = notif.ID
		transactionID = notif.TransactionID
	}

	cfg, err := h.loadStepConfig(ctx, scope, workflowID, t.StepID)
	if err != nil {
		return nil, err
	}

	// base64url-encode each component so distinct (env, subscriber, workflow)
	// tuples cannot collide after eventbus.sanitizeKey maps ':' → '_'.
	// base64url's alphabet (A-Za-z0-9-_) survives sanitizeKey unchanged;
	// the dot separator is also in the allowed set and is not emitted by
	// base64url, so "b64(a).b64(b).b64(c)" is always injective.
	b64 := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	key := b64(scope.EnvID) + "." + b64(subscriberID) + "." + b64(workflowID)
	// An optional throttleKey partitions the budget by payload content, so a
	// flapping check cannot consume the allowance a different alert needs
	// (#295). Absent, or resolving to nothing, leaves the key unchanged --
	// the pre-throttleKey behaviour. The value is b64-encoded like every
	// other segment so two distinct values cannot collide after the key
	// sanitizer runs.
	if v := keypath.Resolve(cfg.throttleKey, payload, nil); v != "" {
		key += "." + b64(v)
	}
	allowed, err := h.throttle.ThrottleAllow(key, cfg.limit, time.Duration(cfg.windowSec)*time.Second)
	if err != nil {
		// Best-effort: a transient counter error does not block the run.
		allowed = true
	}
	if !allowed {
		denyErr := fmt.Errorf("throttle: rate limit exceeded")
		// Suppression is a normal outcome, not a delivery failure: persist a
		// throttled message row (same attribution the inline send-step
		// throttle handlers use, minus provider/integration since no channel
		// ran) before returning the run-stopping error, so "my page never
		// arrived" is diagnosable from the Activity view instead of only from
		// counters. The persist is best-effort -- a store error here must not
		// mask the throttle denial itself.
		h.persistThrottleSuppressed(ctx, scope, workflowID, subscriberID, notificationID, transactionID, t.RunID, t.StepID)
		return nil, engine.NewNonRetryableError(denyErr)
	}
	return nil, nil
}

// throttleSuppressedContent is the terse marker recorded as the message
// content for a standalone check.throttle denial. The step has no authored
// subject/body templates (unlike a send step), so there is nothing to render;
// this stands in for it.
const throttleSuppressedContent = "suppressed: rate limit exceeded"

// persistThrottleSuppressed records a status:"throttled" message for a denied
// standalone throttle step, attributed like the send-step persistThrottled /
// persistChatThrottled / persistEmailThrottled helpers (same scope, workflow,
// subscriber, notification, run, step). Channel/ProviderID/IntegrationID are
// left empty: no channel ran.
//
// It is best-effort -- a store failure must not turn a throttle denial into
// something else -- but the failure is logged rather than swallowed, because
// a silently lost suppression row is the exact undiagnosable state this whole
// change exists to remove. The write also runs on a context detached from the
// dispatch context (which the engine cancels on worker shutdown), so a denial
// during a graceful stop still leaves its trace.
func (h *ThrottleHandler) persistThrottleSuppressed(ctx context.Context, scope domain.Scope, workflowID, subscriberID, notificationID, transactionID, runID, stepID string) {
	metrics.Delivery("throttle", metrics.OutcomeThrottled)
	msg := domain.Message{
		ID:             store.NewULID(),
		OrgID:          scope.OrgID,
		EnvID:          scope.EnvID,
		WorkflowID:     workflowID,
		NotificationID: notificationID,
		RunID:          runID,
		StepID:         stepID,
		SubscriberID:   subscriberID,
		Content:        throttleSuppressedContent,
		Status:         "throttled",
		TransactionID:  transactionID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := h.store.Messages().Create(writeCtx, scope, msg); err != nil {
		slog.Default().Warn("throttle: persist suppressed message",
			"err", err, "run", runID, "step", stepID, "notification", notificationID)
	}
}

// throttleStepCfg holds the flat {limit, windowSec} parsed from a throttle
// step's ControlValues.
type throttleStepCfg struct {
	limit       int
	windowSec   int
	throttleKey string
}

// loadStepConfig loads the workflow by ID, finds the step whose ID matches
// stepID, and parses its {limit, windowSec} ControlValues. Two store queries.
func (h *ThrottleHandler) loadStepConfig(ctx context.Context, scope domain.Scope, workflowID, stepID string) (throttleStepCfg, error) {
	wf, err := h.store.Workflows().GetByID(ctx, scope, workflowID)
	if err != nil {
		return throttleStepCfg{}, fmt.Errorf("throttle: workflow %q: %w", workflowID, err)
	}
	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return throttleStepCfg{}, fmt.Errorf("throttle: unmarshal steps: %w", err)
		}
	}
	var found *domain.Step
	for i := range steps {
		if steps[i].StepID == stepID {
			found = &steps[i]
			break
		}
	}
	if found == nil {
		return throttleStepCfg{}, fmt.Errorf("throttle: step %q not found in workflow %q", stepID, workflowID)
	}
	var raw struct {
		Limit       int    `json:"limit"`
		WindowSec   int    `json:"windowSec"`
		ThrottleKey string `json:"throttleKey"`
	}
	if len(found.ControlValues) > 0 {
		if err := json.Unmarshal(found.ControlValues, &raw); err != nil {
			return throttleStepCfg{}, fmt.Errorf("throttle: unmarshal control values: %w", err)
		}
	}
	return throttleStepCfg{limit: raw.Limit, windowSec: raw.WindowSec, throttleKey: raw.ThrottleKey}, nil
}
