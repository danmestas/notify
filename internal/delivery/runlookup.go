package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// runRowWait bounds how long a task waits for its run's notification row. The
// trigger writes that row right after StartRun returns the run id, and the
// engine may dispatch the first task before it commits.
const runRowWait = 5 * time.Second

// NotificationByRun resolves the notification for a task's run, waiting up to
// runRowWait (or until ctx ends) for the trigger to record it. Any error other
// than not-found returns at once.
func NotificationByRun(ctx context.Context, s *store.Store, runID string) (domain.Notification, error) {
	deadline := time.Now().Add(runRowWait)
	delay := 10 * time.Millisecond
	for {
		n, err := s.Notifications().GetByRunID(ctx, runID)
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
