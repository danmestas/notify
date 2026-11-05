package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// TestNotificationByRun_WaitsForTheTriggersRow pins the run-id race: the run
// id is recorded on the row only after StartRun returns it, and the engine can
// dispatch a task before that commits. The lookup must wait for the row rather
// than fail the step.
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

// envelopeFor is the input a run's first task receives: the trigger's
// envelope, naming the notification the run delivers.
func envelopeFor(t *testing.T, scope domain.Scope, notificationID string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: scope.OrgID, EnvID: scope.EnvID},
		NotificationID: notificationID,
		SubscriberID:   "sub",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// TestNotificationForTask_ResolvesByIDAndRecordsTheRun is #66: the trigger's
// start timed out, so its row has no run id, but dagnats did start the run.
// The first task finds the row by the envelope's notification id and records
// its run id, so a later task with no envelope finds the row by run id.
func TestNotificationForTask_ResolvesByIDAndRecordsTheRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-1"}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID: "ntf_pending", WorkflowID: "wf", SubscriberID: "sub", Status: domain.NotificationPending,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := NotificationForTask(ctx, s, engine.TaskPayload{
		RunID: "run-1", StepID: "S1", Input: envelopeFor(t, scope, "ntf_pending"),
	})
	if err != nil {
		t.Fatalf("first task: %v", err)
	}
	if first.ID != "ntf_pending" || first.RunID != "run-1" || first.Status != domain.NotificationStarted {
		t.Fatalf("first task got %+v, want ntf_pending with run-1, started", first)
	}

	later, err := NotificationForTask(ctx, s, engine.TaskPayload{RunID: "run-1", StepID: "S2"})
	if err != nil {
		t.Fatalf("later task: %v", err)
	}
	if later.ID != "ntf_pending" {
		t.Fatalf("later task got %q, want ntf_pending", later.ID)
	}
}

// TestNotificationForTask_KeepsARecordedRun proves a task never overwrites a
// run id the trigger already recorded.
func TestNotificationForTask_KeepsARecordedRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-1"}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID: "ntf_started", WorkflowID: "wf", SubscriberID: "sub", RunID: "run-a",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := NotificationForTask(ctx, s, engine.TaskPayload{
		RunID: "run-b", StepID: "S1", Input: envelopeFor(t, scope, "ntf_started"),
	})
	if err != nil {
		t.Fatalf("NotificationForTask: %v", err)
	}
	if n.RunID != "run-a" {
		t.Fatalf("run id = %q, want run-a kept", n.RunID)
	}
}

// TestNotificationForTask_LegacyRunResolvesByRunID covers runs whose task
// input names no notification: an envelope without an id, no input at all (a
// step after a delay) and another step's output all resolve by run id.
func TestNotificationForTask_LegacyRunResolvesByRunID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-1"}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID: "ntf_legacy", WorkflowID: "wf", SubscriberID: "sub", RunID: "run-legacy",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for name, input := range map[string]json.RawMessage{
		"envelope without id": envelopeFor(t, scope, ""),
		"no input":            nil,
		"another step output": json.RawMessage(`{"statusCode":200}`),
	} {
		n, err := NotificationForTask(ctx, s, engine.TaskPayload{RunID: "run-legacy", StepID: "S1", Input: input})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n.ID != "ntf_legacy" {
			t.Fatalf("%s: got %q, want ntf_legacy", name, n.ID)
		}
	}
}
