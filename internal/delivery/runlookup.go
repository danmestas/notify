package delivery

import (
	"context"
	"encoding/json"
	"errors"
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
	if err != nil || n.RunID != "" || t.RunID == "" {
		return n, err
	}
	attached, err := s.Notifications().AttachRun(ctx, scope, id, t.RunID)
	if err != nil {
		return domain.Notification{}, err
	}
	if !attached {
		// Another task of the run (or the trigger) recorded it first.
		return s.Notifications().Get(ctx, scope, id)
	}
	n.RunID = t.RunID
	n.Status = domain.NotificationStarted
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
