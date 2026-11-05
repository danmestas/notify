package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// runRowWait bounds how long a task waits for its run's notification row. The
// trigger writes the row before it starts the run, so a row named by the
// envelope is there from the first task on; the wait covers runs started by an
// older trigger, which wrote the row only after StartRun returned, and a row
// whose run id is not yet recorded when a later step looks it up by run id.
const runRowWait = 5 * time.Second

// NotificationForTask resolves the notification a task belongs to.
//
// A task whose input is the trigger's envelope (the run's first steps) names
// its notification directly, so it is found by id even when the trigger never
// learned the run id -- a start that timed out on notify's side but did start
// the run in dagnats (#66). Such a row has no run id yet; the task records its
// own, so the run's later steps, whose input carries no envelope, can find the
// row by run id. Every other task resolves by run id.
func NotificationForTask(ctx context.Context, s *store.Store, t engine.TaskPayload) (domain.Notification, error) {
	scope, id, ok := envelopeNotification(t.Input)
	if !ok {
		return NotificationByRun(ctx, s, t.RunID)
	}
	n, err := waitForRow(ctx, func() (domain.Notification, error) {
		return s.Notifications().Get(ctx, scope, id)
	})
	if err != nil {
		return n, err
	}
	return bindRun(ctx, s, scope, n, t.RunID)
}

// recordRun records the task's run id on the notification its envelope names,
// for the channel and throttle handlers, which deliver straight from the
// envelope and never otherwise read the row. Without it, a run whose start
// timed out in the trigger and whose first step is one of these would leave
// the row without a run id, and every later step without an envelope (one
// after a delay) would fail to find it (#66).
//
// It is best effort: the step it runs in has what it needs from the envelope,
// so a failure here is logged and delivery goes on. It does not wait for a
// missing row; one written by an older trigger after StartRun carries its run
// id already.
func recordRun(ctx context.Context, s *store.Store, t engine.TaskPayload, env runenv.InAppEnvelope) {
	if env.NotificationID == "" || t.RunID == "" {
		return
	}
	scope := domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}
	n, err := s.Notifications().Get(ctx, scope, env.NotificationID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err == nil {
		_, err = bindRun(ctx, s, scope, n, t.RunID)
	}
	if err != nil {
		slog.Default().Warn("delivery: could not record the run on its notification",
			"notification_id", env.NotificationID, "run_id", t.RunID, "step_id", t.StepID, "err", err)
	}
}

// bindRun makes sure notification n names runID: it records runID on a row
// that has no run yet, and returns the row as stored. A row that already names
// another run is returned unchanged, with a warning: two runs delivering one
// notification is a duplicate start the engine should have refused.
func bindRun(ctx context.Context, s *store.Store, scope domain.Scope, n domain.Notification, runID string) (domain.Notification, error) {
	if runID == "" {
		return n, nil
	}
	if n.RunID == "" {
		attached, err := s.Notifications().AttachRun(ctx, scope, n.ID, runID)
		if err != nil {
			return domain.Notification{}, err
		}
		if attached {
			n.RunID = runID
			n.Status = domain.NotificationStarted
			return n, nil
		}
		// Another task of the run (or the trigger) recorded it first.
		if n, err = s.Notifications().Get(ctx, scope, n.ID); err != nil {
			return domain.Notification{}, err
		}
	}
	if n.RunID != runID {
		slog.Default().Warn("delivery: notification already belongs to another run; this run is a duplicate start",
			"notification_id", n.ID, "notification_run_id", n.RunID, "task_run_id", runID)
	}
	return n, nil
}

// NotificationByRun resolves the notification for a task's run, waiting up to
// runRowWait (or until ctx ends) for its run id to be recorded. Any error other
// than not-found returns at once.
func NotificationByRun(ctx context.Context, s *store.Store, runID string) (domain.Notification, error) {
	return waitForRow(ctx, func() (domain.Notification, error) {
		return s.Notifications().GetByRunID(ctx, runID)
	})
}

// waitForRow retries get while it reports not-found, for up to runRowWait or
// until ctx ends.
func waitForRow(ctx context.Context, get func() (domain.Notification, error)) (domain.Notification, error) {
	deadline := time.Now().Add(runRowWait)
	delay := 10 * time.Millisecond
	for {
		n, err := get()
		if !errors.Is(err, store.ErrNotFound) || time.Now().After(deadline) {
			return n, err
		}
		select {
		case <-ctx.Done():
			return n, err
		case <-time.After(delay):
		}
		delay = min(2*delay, 250*time.Millisecond)
	}
}

// envelopeNotification reads the scope and notification id from a task input
// that is the trigger's run envelope. ok is false for any other input: empty
// (a step after a delay), another step's output, or an envelope without an id.
func envelopeNotification(input json.RawMessage) (domain.Scope, string, bool) {
	if len(input) == 0 || !isRunEnvelope(input) {
		return domain.Scope{}, "", false
	}
	var env runenv.InAppEnvelope
	if err := json.Unmarshal(input, &env); err != nil || env.NotificationID == "" {
		return domain.Scope{}, "", false
	}
	return domain.Scope{OrgID: env.Scope.OrgID, EnvID: env.Scope.EnvID}, env.NotificationID, true
}
