package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	cv, _, _, err := d.stepInputs(ctx, t.RunID, t.StepID)
	if err != nil {
		return nil, err
	}
	var cfg steptype.ScheduledConfig
	if len(cv) > 0 {
		if uErr := json.Unmarshal(cv, &cfg); uErr != nil {
			return nil, fmt.Errorf("delay.scheduled: unmarshal controlValues: %w", uErr)
		}
	}
	// Schedule in the SUBSCRIBER's timezone: an authored "09:00" means 09:00
	// where they are. A subscriber with no timezone (the column has defaulted to
	// empty since the first migration) or an unparseable one resolves to UTC, so
	// scheduling is unchanged for everyone who has never had one set.
	fire, err := steptype.NextScheduledFireIn(cfg, time.Now(), d.subscriberLocation(ctx, t.RunID))
	if err != nil {
		return nil, fmt.Errorf("delay.scheduled: %w", err)
	}
	return waitUntil(ctx, time.Until(fire))
}

// HandleDynamic is registered via engine.Handle("delay.dynamic", ...). It loads
// the step's dynamicKey and the trigger payload, resolves the fire-at instant
// (ISO-8601 timestamp, or a persisted/newly-resolved {amount,unit} deadline —
// see resolveDynamicFireAt), waits, and on normal completion clears any
// deadline row so the common path does not accumulate one.
func (d *DelayWait) HandleDynamic(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	cv, payload, scope, err := d.stepInputs(ctx, t.RunID, t.StepID)
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
	fire, err := d.resolveDynamicFireAt(ctx, scope, t.RunID, t.StepID, payload, sc.DynamicKey, sc.DynamicKind, time.Now())
	if err != nil {
		return nil, fmt.Errorf("delay.dynamic: %w", err)
	}
	out, waitErr := waitUntil(ctx, time.Until(fire))
	if waitErr != nil {
		// Context cancelled mid-wait (shutdown): leave any persisted deadline
		// in place so the requeued dispatch resumes toward it instead of
		// re-resolving "now + duration".
		return out, waitErr
	}
	if clearErr := d.store.DelayDeadlines().Clear(ctx, t.RunID, t.StepID); clearErr != nil {
		return nil, fmt.Errorf("delay.dynamic: clear deadline: %w", clearErr)
	}
	return out, nil
}

// resolveDynamicFireAt resolves the absolute instant HandleDynamic should
// wait until, honouring a previously persisted deadline for relative delays.
//
// The instant form (dynamicKind=instant) always recomputes the same
// wall-clock target from the payload's RFC3339 value, so it needs no
// persistence and writes no deadline row.
//
// The relative form (dynamicKind=relative) carries a payload DURATION, not a
// wall-clock instant, so it must be pinned to a fixed instant on first
// dispatch: read any previously persisted deadline for (runID, stepID) first,
// and only resolve+persist a new one when none exists yet. Reusing the
// persisted deadline on every subsequent requeue is what stops a restart from
// restarting the full wait (#273).
func (d *DelayWait) resolveDynamicFireAt(ctx context.Context, scope domain.Scope, runID, stepID string, payload json.RawMessage, dynamicKey, dynamicKind string, now time.Time) (time.Time, error) {
	if dynamicKind == steptype.DynamicKindRelative {
		deadline, err := d.store.DelayDeadlines().Get(ctx, runID, stepID)
		if err == nil {
			return deadline, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return time.Time{}, fmt.Errorf("delay deadline: %w", err)
		}
	}

	dur, err := resolveDynamicDuration(payload, dynamicKey, dynamicKind, now)
	if err != nil {
		return time.Time{}, err
	}
	fire := now.Add(dur)

	if dynamicKind == steptype.DynamicKindRelative {
		if saveErr := d.store.DelayDeadlines().Save(ctx, scope, runID, stepID, fire); saveErr != nil {
			return time.Time{}, fmt.Errorf("delay deadline: save: %w", saveErr)
		}
	}
	return fire, nil
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
	node, err := steptype.PayloadValue(payload, dynamicKey)
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
		// This is a DURATION relative to now, not a wall-clock instant: it is
		// only safe to call once, at first dispatch. resolveDynamicFireAt is
		// the caller that enforces that by persisting the resulting instant
		// and reusing it on every requeue (#273).
		return steptype.RelativeDuration(node, dynamicKey)
	default:
		return 0, fmt.Errorf("unknown dynamicKind %q", dynamicKind)
	}
}

// stepInputs loads a delay step's controlValues, the run's trigger payload,
// and its tenancy scope by run id + step id, mirroring the digest.wait
// stepCron lookup (notification -> workflow -> step). RebuildEnvelope returns
// the channel handler's envelope, which carries neither the calendar config
// nor the raw payload these handlers need, so this narrow lookup resolves
// them directly. Scope is returned alongside so HandleDynamic can pass it to
// DelayDeadlines().Save without a second notification lookup.
func (d *DelayWait) stepInputs(ctx context.Context, runID, stepID string) (json.RawMessage, json.RawMessage, domain.Scope, error) {
	notif, err := NotificationByRun(ctx, d.store, runID)
	if err != nil {
		return nil, nil, domain.Scope{}, fmt.Errorf("delay: notification by run %q: %w", runID, err)
	}
	scope := domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}
	wf, err := d.store.Workflows().GetByID(ctx, scope, notif.WorkflowID)
	if err != nil {
		return nil, nil, domain.Scope{}, fmt.Errorf("delay: workflow %q: %w", notif.WorkflowID, err)
	}
	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return nil, nil, domain.Scope{}, fmt.Errorf("delay: unmarshal steps: %w", err)
		}
	}
	for i := range steps {
		if steps[i].StepID == stepID {
			return steps[i].ControlValues, notif.Payload, scope, nil
		}
	}
	return nil, nil, domain.Scope{}, fmt.Errorf("delay: step %q not found in workflow %q", stepID, notif.WorkflowID)
}

// subscriberLocation resolves the run's subscriber timezone to a location.
//
// Every failure path returns UTC rather than an error: this only decides WHEN a
// send fires, and a lookup problem must not turn a scheduled delivery into a
// dropped one. The previous behaviour was UTC for everyone, so UTC is also the
// correct thing to fall back to.
func (d *DelayWait) subscriberLocation(ctx context.Context, runID string) *time.Location {
	notif, err := NotificationByRun(ctx, d.store, runID)
	if err != nil {
		return time.UTC
	}
	sub, err := d.store.Subscribers().GetByExternalID(ctx,
		domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}, notif.SubscriberID)
	if err != nil {
		return time.UTC
	}
	return steptype.ResolveLocation(sub.Timezone)
}
