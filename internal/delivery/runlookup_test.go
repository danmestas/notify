package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// TestNotificationByRun_WaitsForTheTriggersRow pins the trigger race: the
// trigger records the notification only after StartRun returns its run id, and
// the engine can dispatch the run's first task before that row commits. The
// lookup must wait for the row rather than fail the step.
func TestNotificationByRun_WaitsForTheTriggersRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-1"}

	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = s.Notifications().Create(ctx, scope, domain.Notification{
			ID: "ntf_late", WorkflowID: "wf", SubscriberID: "sub", RunID: "run-late",
		})
	}()

	n, err := NotificationByRun(ctx, s, "run-late")
	if err != nil {
		t.Fatalf("NotificationByRun: %v", err)
	}
	if n.ID != "ntf_late" {
		t.Fatalf("notification id = %q, want ntf_late", n.ID)
	}
}

// TestNotificationByRun_GivesUp proves a run that never gets a row still fails,
// with the store's not-found error, once the wait is over.
func TestNotificationByRun_GivesUp(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := NotificationByRun(ctx, s, "no-such-run")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}
