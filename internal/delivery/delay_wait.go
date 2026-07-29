package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// DelayWait is the engine handler for scheduled and dynamic delays. Like
// digest.wait, these waits depend on wall-clock time (a calendar schedule) or
// the trigger payload (a payload-sourced timestamp/duration), so the duration
// cannot be baked into a static "sleep" StepDef at compile time — the handler
// resolves it at dispatch time and waits, context-aware. Regular delays
// (amount+unit, including weeks/months) use the engine's native sleep and need
// no handler.
type DelayWait struct {
	store *store.Store
}

// NewDelayWait constructs a DelayWait backed by s.
func NewDelayWait(s *store.Store) *DelayWait { return &DelayWait{store: s} }

// HandleScheduled is registered via engine.Handle("delay.scheduled", ...). It
// loads the step's structured calendar (atTime + weekDays/monthDays), computes
// the next firing instant after now (UTC), and waits until it. A nil output
// lets the downstream channel step fire.
func (d *DelayWait) HandleScheduled(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	cv, _, err := d.stepInputs(ctx, t.RunID, t.StepID)
	if err != nil {
		return nil, err
	}
	var cfg steptype.ScheduledConfig
	if len(cv) > 0 {
		if uErr := json.Unmarshal(cv, &cfg); uErr != nil {
			return nil, fmt.Errorf("delay.scheduled: unmarshal controlValues: %w", uErr)
		}
	}
	fire, err := steptype.NextScheduledFire(cfg, time.Now())
	if err != nil {
		return nil, fmt.Errorf("delay.scheduled: %w", err)
	}
	return waitUntil(ctx, time.Until(fire))
}

// HandleDynamic is registered via engine.Handle("delay.dynamic", ...). It loads
// the step's dynamicKey and the trigger payload, resolves the wait duration
// (ISO-8601 timestamp -> wait-until, or an {amount,unit} object -> duration),
// and waits.
func (d *DelayWait) HandleDynamic(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	cv, payload, err := d.stepInputs(ctx, t.RunID, t.StepID)
	if err != nil {
		return nil, err
	}
	var sc struct {
		DynamicKey  string `json:"dynamicKey"`
		DynamicKind string `json:"dynamicKind"`
	}
	if len(cv) > 0 {
		if uErr := json.Unmarshal(cv, &sc); uErr != nil {
			return nil, fmt.Errorf("delay.dynamic: unmarshal controlValues: %w", uErr)
		}
	}
	dur, err := resolveDynamicDuration(payload, sc.DynamicKey, sc.DynamicKind, time.Now())
	if err != nil {
		return nil, fmt.Errorf("delay.dynamic: %w", err)
	}
	return waitUntil(ctx, dur)
}

// waitUntil blocks for d, honouring context cancellation. On cancellation (the
// engine's loop ctx is cancelled at Stop()) it returns a RetryableError so the
// goroutine releases its WaitGroup slot promptly — a bare time.Sleep would pin
// the goroutine for the full delay and hang Stop(). The RetryableError (paired
// with the step's RetryPolicy) makes dagnats requeue the task; on re-dispatch
// after restart the handler recomputes the remaining wait. A non-positive
// duration returns immediately.
func waitUntil(ctx context.Context, d time.Duration) (json.RawMessage, error) {
	if d <= 0 {
		return nil, nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, engine.NewRetryableError(ctx.Err())
	case <-timer.C:
		return nil, nil
	}
}

// resolveDynamicDuration navigates dynamicKey into the trigger payload and
// interprets the value ACCORDING TO THE STEP'S DECLARED KIND, not according to
// the value's shape.
//
// The step declares kind at authoring time (steptype.DynamicKindInstant or
// DynamicKindRelative). Previously this function sniffed: a string was treated
// as an instant, an object as a relative duration. That made a step's behaviour
// a property of each payload rather than of the workflow, so the same authored
// delay could wait until Tuesday for one trigger and for three hours on the
// next -- and it is the reason these steps could not compile to the engine's
// native sleep, since compile time cannot know which shape will arrive (#258).
//
// A mismatch between the declared kind and the actual value is now an error
// rather than a silent reinterpretation: the workflow said what it meant, so a
// payload that disagrees is bad data and should say so.
func resolveDynamicDuration(payload json.RawMessage, dynamicKey, dynamicKind string, now time.Time) (time.Duration, error) {
	if dynamicKey == "" {
		return 0, fmt.Errorf("empty dynamicKey")
	}
	if dynamicKind == "" {
		return 0, fmt.Errorf("step declares no dynamicKind; republish the workflow to record whether %q is an instant or a relative duration", dynamicKey)
	}
	node, err := navigatePayload(payload, dynamicKey)
	if err != nil {
		return 0, err
	}

	switch dynamicKind {
	case steptype.DynamicKindInstant:
		var str string
		if uErr := json.Unmarshal(node, &str); uErr != nil || str == "" {
			return 0, fmt.Errorf("value at %q must be an RFC3339 string (step declares dynamicKind=%q)", dynamicKey, steptype.DynamicKindInstant)
		}
		ts, perr := time.Parse(time.RFC3339, str)
		if perr != nil {
			return 0, fmt.Errorf("value at %q (%q) is not an RFC3339 timestamp: %w", dynamicKey, str, perr)
		}
		// An instant already past means "send now", not "error": the workflow
		// asked to wait until a moment that has arrived.
		if rem := ts.Sub(now); rem > 0 {
			return rem, nil
		}
		return 0, nil

	case steptype.DynamicKindRelative:
		var obj struct {
			Amount int64  `json:"amount"`
			Unit   string `json:"unit"`
		}
		if uErr := json.Unmarshal(node, &obj); uErr != nil {
			return 0, fmt.Errorf("value at %q must be an {amount,unit} object (step declares dynamicKind=%q)", dynamicKey, steptype.DynamicKindRelative)
		}
		if obj.Amount <= 0 {
			return 0, fmt.Errorf("value at %q has non-positive amount %d", dynamicKey, obj.Amount)
		}
		unit, uErr := steptype.UnitDuration(obj.Unit)
		if uErr != nil {
			return 0, uErr
		}
		// Known limitation (#164): a relative delay is resolved from time.Now()
		// on every dispatch, so a requeue after restart restarts the full
		// duration. Absolute forms resume correctly. Fixing it needs the
		// original fire-at persisted, which is a separate change.
		return time.Duration(obj.Amount) * unit, nil

	default:
		return 0, fmt.Errorf("unknown dynamicKind %q", dynamicKind)
	}
}

// navigatePayload walks a "payload.a.b" dot-path against the trigger payload and
// returns the raw JSON value at the leaf. The first segment must be "payload";
// a missing/non-object intermediate or leaf is an error.
func navigatePayload(payload json.RawMessage, dynamicKey string) (json.RawMessage, error) {
	parts := strings.Split(dynamicKey, ".")
	if len(parts) < 2 || parts[0] != "payload" {
		return nil, fmt.Errorf("dynamicKey %q must be a payload dot-path (e.g. payload.sendAt)", dynamicKey)
	}
	cur := payload
	for _, p := range parts[1:] {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(cur, &m); err != nil {
			return nil, fmt.Errorf("dynamicKey %q: %q is not inside an object", dynamicKey, p)
		}
		next, ok := m[p]
		if !ok {
			return nil, fmt.Errorf("dynamicKey %q: missing %q in payload", dynamicKey, p)
		}
		cur = next
	}
	return cur, nil
}

// stepInputs loads a delay step's controlValues and the run's trigger payload by
// run id + step id, mirroring the digest.wait stepCron lookup
// (notification -> workflow -> step). RebuildEnvelope returns the channel
// handler's envelope, which carries neither the calendar config nor the raw
// payload these handlers need, so this narrow lookup resolves them directly.
func (d *DelayWait) stepInputs(ctx context.Context, runID, stepID string) (json.RawMessage, json.RawMessage, error) {
	notif, err := d.store.Notifications().GetByRunID(ctx, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("delay: notification by run %q: %w", runID, err)
	}
	scope := domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}
	wf, err := d.store.Workflows().GetByID(ctx, scope, notif.WorkflowID)
	if err != nil {
		return nil, nil, fmt.Errorf("delay: workflow %q: %w", notif.WorkflowID, err)
	}
	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return nil, nil, fmt.Errorf("delay: unmarshal steps: %w", err)
		}
	}
	for i := range steps {
		if steps[i].StepID == stepID {
			return steps[i].ControlValues, notif.Payload, nil
		}
	}
	return nil, nil, fmt.Errorf("delay: step %q not found in workflow %q", stepID, notif.WorkflowID)
}
