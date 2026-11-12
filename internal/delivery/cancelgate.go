package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

// ErrNotificationCancelled is the step error of a task whose notification was
// cancelled: the step stops without doing anything.
var ErrNotificationCancelled = errors.New("delivery: notification cancelled")

// RunCanceller cancels engine runs. *engine.Engine satisfies it.
type RunCanceller interface {
	CancelRun(ctx context.Context, runID string) error
}

// StopCancelled wraps a step handler so a task whose notification is cancelled
// cancels its run and fails without running fn.
//
// The cancel endpoint can mark a row cancelled while it has no run id: its
// start timed out, yet dagnats may have started the run. That run's first
// task records its run id on the row here and finds it cancelled, so the run
// is stopped before any step sends. It also covers any task of a cancelled run
// that dagnats dispatched before it handled the cancel.
//
// The row is looked up without waiting: a task with no row yet goes on to fn,
// whose own lookup waits for it.
func StopCancelled(s *store.Store, runs RunCanceller, fn engine.HandlerFunc) engine.HandlerFunc {
	if s == nil || runs == nil || fn == nil {
		panic("delivery.StopCancelled: store, runs and fn must not be nil")
	}
	return func(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
		if err := stopIfCancelled(ctx, s, runs, t); err != nil {
			return nil, err
		}
		return fn(ctx, t)
	}
}

func stopIfCancelled(ctx context.Context, s *store.Store, runs RunCanceller, t engine.TaskPayload) error {
	if t.RunID == "" {
		return nil
	}
	n, err := taskRow(ctx, s, t)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return engine.NewRetryableError(fmt.Errorf("delivery: read notification of run %s: %w", t.RunID, err))
	}
	if n.Status != domain.NotificationCancelled {
		return nil
	}
	if n.RunID == "" {
		scope := domain.Scope{OrgID: n.OrgID, EnvID: n.EnvID}
		if _, err := s.Notifications().AttachRun(ctx, scope, n.ID, t.RunID); err != nil {
			return engine.NewRetryableError(fmt.Errorf("delivery: record run %s on cancelled notification %s: %w", t.RunID, n.ID, err))
		}
	}
	if err := runs.CancelRun(ctx, t.RunID); err != nil {
		return engine.NewRetryableError(fmt.Errorf("delivery: cancel run %s of cancelled notification %s: %w", t.RunID, n.ID, err))
	}
	return engine.NewNonRetryableError(fmt.Errorf("%w: notification %s, run %s", ErrNotificationCancelled, n.ID, t.RunID))
}

// taskRow is NotificationForTask without its wait or its run binding: the row
// the task's envelope names, or else the row naming the task's run.
func taskRow(ctx context.Context, s *store.Store, t engine.TaskPayload) (domain.Notification, error) {
	if scope, id, ok := envelopeNotification(t.Input); ok {
		return s.Notifications().Get(ctx, scope, id)
	}
	return s.Notifications().GetByRunID(ctx, t.RunID)
}
