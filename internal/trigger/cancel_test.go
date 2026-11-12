package trigger_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/timestamp"
	"github.com/danmestas/notify/internal/trigger"
)

func (f *fakeEngine) GetRun(_ context.Context, runID string) (engine.RunSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runs[runID]; !ok {
		return engine.RunSnapshot{}, fmt.Errorf("%w: engine: workflow run not found", engine.ErrRunNotFound)
	}
	status := f.status[runID]
	if status == "" {
		status = "running"
	}
	return engine.RunSnapshot{RunID: runID, Status: status}, nil
}

func (f *fakeEngine) CancelRun(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runs[runID]; !ok {
		return fmt.Errorf("%w: engine: workflow run not found", engine.ErrRunNotFound)
	}
	f.cancels = append(f.cancels, runID)
	f.status[runID] = "cancelled"
	return nil
}

func (f *fakeEngine) setStatus(runID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[runID] = status
}

func (f *fakeEngine) cancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cancels)
}

func (fx startFixture) cancel(t *testing.T) trigger.CancelResult {
	t.Helper()
	res, err := fx.svc.Cancel(context.Background(), fx.scope, "tx-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	return res
}

// TestCancel_RunningRun cancels the run in dagnats and marks the row
// cancelled; a second cancel reports the same and asks dagnats nothing more.
func TestCancel_RunningRun(t *testing.T) {
	fx := setupFakeEngine(t)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	row := fx.onlyRow(t)

	for i := range 2 {
		res := fx.cancel(t)
		if !slices.Equal(res.Cancelled, []string{row.ID}) || len(res.AlreadyFinished) != 0 {
			t.Fatalf("Cancel[%d] = %+v, want cancelled [%s]", i, res, row.ID)
		}
	}
	if got := fx.eng.cancelled(); !slices.Equal(got, []string{"run-1"}) {
		t.Fatalf("dagnats cancels = %v, want [run-1] once", got)
	}
	if got := fx.onlyRow(t); got.Status != domain.NotificationCancelled {
		t.Fatalf("row status = %q, want cancelled", got.Status)
	}
}

// TestCancel_FinishedRun reports a run that already finished, leaves its row as
// it was and cancels nothing.
func TestCancel_FinishedRun(t *testing.T) {
	fx := setupFakeEngine(t)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	row := fx.onlyRow(t)
	fx.eng.setStatus("run-1", "completed")

	res := fx.cancel(t)
	if len(res.Cancelled) != 0 || !slices.Equal(res.AlreadyFinished, []string{row.ID}) {
		t.Fatalf("Cancel = %+v, want alreadyFinished [%s]", res, row.ID)
	}
	if got := fx.eng.cancelled(); len(got) != 0 {
		t.Fatalf("dagnats cancels = %v, want none", got)
	}
	if got := fx.onlyRow(t); got.Status != domain.NotificationStarted {
		t.Fatalf("row status = %q, want started", got.Status)
	}
}

// TestCancel_PendingRowWithoutRun: a row whose start timed out before any run
// was recorded is cancelled without asking dagnats. The stale-pending sweep
// leaves it cancelled, and a retried trigger starts nothing.
func TestCancel_PendingRowWithoutRun(t *testing.T) {
	fx := setupFakeEngine(t)
	ctx := context.Background()
	fx.eng.setFail(func(bool) error {
		return fmt.Errorf("nats request: %w", context.DeadlineExceeded)
	})
	if err := fx.ingest(t); err == nil {
		t.Fatal("Ingest: want the start's timeout")
	}
	row := fx.onlyRow(t)
	if row.Status != domain.NotificationPending {
		t.Fatalf("row = %+v, want pending", row)
	}

	res := fx.cancel(t)
	if !slices.Equal(res.Cancelled, []string{row.ID}) {
		t.Fatalf("Cancel = %+v, want cancelled [%s]", res, row.ID)
	}
	if got := fx.eng.cancelled(); len(got) != 0 {
		t.Fatalf("dagnats cancels = %v, want none", got)
	}

	if _, err := fx.st.Notifications().FailStalePending(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("FailStalePending: %v", err)
	}
	fx.eng.setFail(nil)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("retried Ingest: %v", err)
	}
	if n := fx.eng.runCount(); n != 0 {
		t.Fatalf("runs after retry = %d, want 0", n)
	}
	if got := fx.onlyRow(t); got.Status != domain.NotificationCancelled {
		t.Fatalf("row status = %q, want cancelled", got.Status)
	}
}

// TestCancel_RefusedStart: a row whose start the engine refused is cancelled
// without asking dagnats, and a retried trigger then starts nothing.
func TestCancel_RefusedStart(t *testing.T) {
	fx := setupFakeEngine(t)
	fx.eng.setFail(func(started bool) error {
		if !started {
			return fmt.Errorf("%w: engine: workflow not found", engine.ErrStartRefused)
		}
		return nil
	})
	if err := fx.ingest(t); err == nil {
		t.Fatal("Ingest: want the refusal")
	}
	row := fx.onlyRow(t)
	if res := fx.cancel(t); !slices.Equal(res.Cancelled, []string{row.ID}) {
		t.Fatalf("Cancel = %+v, want cancelled [%s]", res, row.ID)
	}
	fx.eng.setFail(nil)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("retried Ingest: %v", err)
	}
	if n := fx.eng.runCount(); n != 0 {
		t.Fatalf("runs after retry = %d, want 0", n)
	}
}

// TestCancel_DuringStart: a cancel that lands while the trigger's start is in
// flight finds the row with no run and marks it cancelled; the trigger then
// records the run, sees the row cancelled and cancels the run itself.
func TestCancel_DuringStart(t *testing.T) {
	fx := setupFakeEngine(t)
	var res trigger.CancelResult
	var cerr error
	fx.eng.setFail(func(started bool) error {
		if started {
			res, cerr = fx.svc.Cancel(context.Background(), fx.scope, "tx-1")
		}
		return nil
	})
	if err := fx.ingest(t); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if cerr != nil || len(res.Cancelled) != 1 {
		t.Fatalf("Cancel during start = %+v, %v; want one cancelled", res, cerr)
	}
	if got := fx.eng.cancelled(); !slices.Equal(got, []string{"run-1"}) {
		t.Fatalf("dagnats cancels = %v, want [run-1]", got)
	}
	if row := fx.onlyRow(t); row.Status != domain.NotificationCancelled || row.RunID != "run-1" {
		t.Fatalf("row = %+v, want cancelled on run-1", row)
	}
}

// TestCancel_RunNotFound: dagnats does not know the run. A fresh row's run is
// started but not yet recorded (dagnats#1006), so the cancel fails for a retry;
// an hour-old row's run has been pruned, so it finished.
func TestCancel_RunNotFound(t *testing.T) {
	fx := setupFakeEngine(t)
	ctx := context.Background()
	fresh, err := fx.st.Notifications().Create(ctx, fx.scope, domain.Notification{
		WorkflowID: "wf", SubscriberID: "user_1", TransactionID: "tx-fresh", RunID: "run-unseen",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := fx.svc.Cancel(ctx, fx.scope, "tx-fresh"); !errors.Is(err, trigger.ErrRunNotVisible) {
		t.Fatalf("Cancel fresh = %v, want ErrRunNotVisible", err)
	}
	if got, _ := fx.st.Notifications().Get(ctx, fx.scope, fresh.ID); got.Status != domain.NotificationStarted {
		t.Fatalf("fresh row status = %q, want started", got.Status)
	}

	old, err := fx.st.Notifications().Create(ctx, fx.scope, domain.Notification{
		WorkflowID: "wf", SubscriberID: "user_1", TransactionID: "tx-old", RunID: "run-pruned",
		CreatedAt: timestamp.Format(time.Now().Add(-2 * time.Hour)),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := fx.svc.Cancel(ctx, fx.scope, "tx-old")
	if err != nil || !slices.Equal(res.AlreadyFinished, []string{old.ID}) {
		t.Fatalf("Cancel old = %+v, %v; want alreadyFinished [%s]", res, err, old.ID)
	}
}

// TestCancel_UnknownTransaction is not found, including from another
// environment of the same org.
func TestCancel_UnknownTransaction(t *testing.T) {
	fx := setupFakeEngine(t)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	ctx := context.Background()
	if _, err := fx.svc.Cancel(ctx, fx.scope, "tx-nope"); !errors.Is(err, trigger.ErrTransactionNotFound) {
		t.Fatalf("Cancel unknown = %v, want ErrTransactionNotFound", err)
	}
	other := domain.Scope{OrgID: fx.scope.OrgID, EnvID: "env_2"}
	if _, err := fx.svc.Cancel(ctx, other, "tx-1"); !errors.Is(err, trigger.ErrTransactionNotFound) {
		t.Fatalf("Cancel from another env = %v, want ErrTransactionNotFound", err)
	}
	if got := fx.eng.cancelled(); len(got) != 0 {
		t.Fatalf("dagnats cancels = %v, want none", got)
	}
}
