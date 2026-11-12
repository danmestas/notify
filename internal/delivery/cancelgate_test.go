package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
)

type recordingCanceller struct {
	mu   sync.Mutex
	runs []string
}

func (c *recordingCanceller) CancelRun(_ context.Context, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs = append(c.runs, runID)
	return nil
}

// TestStopCancelled covers #144's step-side guard: a task of a cancelled
// notification, found through its envelope or its run, cancels its run and
// never reaches the handler; any other task does.
func TestStopCancelled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-1"}
	envelope := func(notificationID string) json.RawMessage {
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
	mk := func(id, runID, status string) {
		t.Helper()
		if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
			ID: id, WorkflowID: "wf", SubscriberID: "sub", TransactionID: "tx", RunID: runID, Status: status,
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	// A start that timed out after dagnats started the run: the row was
	// cancelled with no run id.
	mk("ntf_orphan", "", domain.NotificationCancelled)
	mk("ntf_late", "run-late", domain.NotificationCancelled)
	mk("ntf_live", "run-live", domain.NotificationStarted)

	tests := []struct {
		name      string
		task      engine.TaskPayload
		wantRun   bool
		wantStops bool
	}{
		{"orphaned run's first step", engine.TaskPayload{RunID: "run-orphan", StepID: "E1", Input: envelope("ntf_orphan")}, false, true},
		{"later step of a cancelled run", engine.TaskPayload{RunID: "run-late", StepID: "E1"}, false, true},
		{"live notification", engine.TaskPayload{RunID: "run-live", StepID: "E1", Input: envelope("ntf_live")}, true, false},
		{"no notification", engine.TaskPayload{RunID: "run-other", StepID: "E1"}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runs := &recordingCanceller{}
			ran := false
			h := StopCancelled(s, runs, func(context.Context, engine.TaskPayload) (json.RawMessage, error) {
				ran = true
				return nil, nil
			})
			_, err := h(ctx, tc.task)
			if ran != tc.wantRun {
				t.Fatalf("handler ran = %v, want %v", ran, tc.wantRun)
			}
			if !tc.wantStops {
				if err != nil || len(runs.runs) != 0 {
					t.Fatalf("err = %v, cancels = %v; want neither", err, runs.runs)
				}
				return
			}
			var permanent *engine.NonRetryableError
			if !errors.Is(err, ErrNotificationCancelled) || !errors.As(err, &permanent) {
				t.Fatalf("err = %v, want a permanent ErrNotificationCancelled", err)
			}
			if !slices.Equal(runs.runs, []string{tc.task.RunID}) {
				t.Fatalf("cancels = %v, want [%s]", runs.runs, tc.task.RunID)
			}
		})
	}

	got, err := s.Notifications().Get(ctx, scope, "ntf_orphan")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunID != "run-orphan" || got.Status != domain.NotificationCancelled {
		t.Fatalf("orphan row = run %q status %q, want run-orphan, cancelled", got.RunID, got.Status)
	}
}
