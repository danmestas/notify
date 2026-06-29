package delivery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// TaskThrottle is the engine task name that the throttle step type's Compile
// emits. cmd/notify/main.go must register an eng.Handle for this name; if the
// handler is absent, the engine permanently fails any run that contains a
// throttle step with a matching error, making the omission immediately visible.
const TaskThrottle = "check.throttle"

// ThrottleHandler implements the check.throttle engine task. It gates an entire
// run by a fixed-window counter keyed per (envID, subscriberID, workflowID).
// Under the configured limit it returns (nil, nil) so downstream steps proceed;
// at or over the limit it returns a NonRetryableError that permanently fails the
// run — no channel steps execute and no message row is written.
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
		scope        domain.Scope
		workflowID   string
		subscriberID string
	)

	if len(t.Input) != 0 {
		var env runenv.InAppEnvelope
		if err := json.Unmarshal(t.Input, &env); err != nil {
			return nil, fmt.Errorf("throttle: unmarshal input: %w", err)
		}
		scope = domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
		workflowID = env.WorkflowID
		subscriberID = env.SubscriberID
	} else {
		notif, err := h.store.Notifications().GetByRunID(ctx, t.RunID)
		if err != nil {
			return nil, fmt.Errorf("throttle: notification by run %q: %w", t.RunID, err)
		}
		scope = domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}
		workflowID = notif.WorkflowID
		subscriberID = notif.SubscriberID
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
	allowed, err := h.throttle.ThrottleAllow(key, cfg.limit, time.Duration(cfg.windowSec)*time.Second)
	if err != nil {
		// Best-effort: a transient counter error does not block the run.
		allowed = true
	}
	if !allowed {
		return nil, engine.NewNonRetryableError(fmt.Errorf("throttle: rate limit exceeded"))
	}
	return nil, nil
}

// throttleStepCfg holds the flat {limit, windowSec} parsed from a throttle
// step's ControlValues.
type throttleStepCfg struct {
	limit     int
	windowSec int
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
		Limit     int `json:"limit"`
		WindowSec int `json:"windowSec"`
	}
	if len(found.ControlValues) > 0 {
		if err := json.Unmarshal(found.ControlValues, &raw); err != nil {
			return throttleStepCfg{}, fmt.Errorf("throttle: unmarshal control values: %w", err)
		}
	}
	return throttleStepCfg{limit: raw.Limit, windowSec: raw.WindowSec}, nil
}
