package trigger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// fakeEngine starts runs the way dagnats does for a workflow with an
// idempotency key: a start whose envelope carries a key seen before answers
// with that key's run instead of starting another. fail, when set, decides
// what each start returns after (or instead of) starting the run.
type fakeEngine struct {
	mu     sync.Mutex
	runs   map[string]json.RawMessage // run id -> input
	byKey  map[string]string          // idempotency key -> run id
	starts int
	fail   func(started bool) error
	// status is a run's dagnats status; a run absent here is running.
	status  map[string]string
	cancels []string
	// cancelErr, when set, is every CancelRun's answer.
	cancelErr error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{runs: map[string]json.RawMessage{}, byKey: map[string]string{}, status: map[string]string{}}
}

func (f *fakeEngine) StartRun(_ context.Context, _ string, input json.RawMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.fail != nil {
		if err := f.fail(false); err != nil {
			return "", err
		}
	}
	var env runenv.InAppEnvelope
	if err := json.Unmarshal(input, &env); err != nil {
		return "", err
	}
	runID, ok := f.byKey[env.IdempotencyKey]
	if !ok {
		runID = fmt.Sprintf("run-%d", len(f.runs)+1)
		f.runs[runID] = input
		f.byKey[env.IdempotencyKey] = runID
	}
	if f.fail != nil {
		if err := f.fail(true); err != nil {
			return "", err
		}
	}
	return runID, nil
}

func (f *fakeEngine) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeEngine) input(runID string) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[runID]
}

func (f *fakeEngine) setFail(fn func(started bool) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}

// timeoutAfterStart is #66: dagnats starts the run, but notify stops waiting
// for the reply first.
func timeoutAfterStart(started bool) error {
	if started {
		return fmt.Errorf("nats request %q: %w", "api.p.notify.runs.start", context.DeadlineExceeded)
	}
	return nil
}

type startFixture struct {
	svc   *trigger.Service
	eng   *fakeEngine
	st    *store.Store
	scope domain.Scope
}

func setupFakeEngine(t *testing.T) startFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "start.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	steps, err := json.Marshal([]domain.Step{
		{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Hi","body":"first"}`)},
		{StepID: "S2", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Hi","body":"second"}`)},
	})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name: "Welcome", TriggerIdentifier: testTrigger, Active: true, Steps: steps,
	}); err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}
	eng := newFakeEngine()
	return startFixture{svc: trigger.New(audience.New(st), st, eng), eng: eng, st: st, scope: scope}
}

func (fx startFixture) ingest(t *testing.T) error {
	t.Helper()
	_, err := fx.svc.Ingest(context.Background(), fx.scope, trigger.TriggerRequest{
		Name:          testTrigger,
		To:            trigger.ToField{SubscriberID: "user_1"},
		Payload:       json.RawMessage(`{}`),
		TransactionID: "tx-1",
	})
	return err
}

// rows returns every notification row the trigger wrote for tx-1.
func (fx startFixture) rows(t *testing.T) []domain.Notification {
	t.Helper()
	page, err := fx.st.Notifications().List(context.Background(), fx.scope,
		store.NotificationFilter{TransactionID: "tx-1"}, "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return page.Items
}

func (fx startFixture) onlyRow(t *testing.T) domain.Notification {
	t.Helper()
	rows := fx.rows(t)
	if len(rows) != 1 {
		t.Fatalf("got %d notification rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// TestIngest_StartTimesOutAfterRunStarted is #66 end to end: the start times
// out after dagnats started the run. The row the trigger wrote first is
// pending, the run's first step finds it through the envelope and records the
// run id, a later step with no envelope then finds it by run id, and a retried
// trigger starts nothing new.
func TestIngest_StartTimesOutAfterRunStarted(t *testing.T) {
	fx := setupFakeEngine(t)
	ctx := context.Background()
	fx.eng.setFail(timeoutAfterStart)

	if err := fx.ingest(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ingest err = %v, want the start's timeout", err)
	}
	if n := fx.eng.runCount(); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	row := fx.onlyRow(t)
	if row.Status != domain.NotificationPending || row.RunID != "" {
		t.Fatalf("row after timeout = %+v, want pending with no run id", row)
	}

	// The run's first step is a channel step, which delivers straight from its
	// envelope input; it must still record its run on the row.
	inApp := delivery.New(fx.st)
	if _, err := inApp.HandleInApp(ctx, engine.TaskPayload{
		RunID: "run-1", StepID: "S1", Input: fx.eng.input("run-1"),
	}); err != nil {
		t.Fatalf("first step: %v", err)
	}
	// A later step whose input carries no envelope (it follows a delay) finds
	// the row by run id.
	if _, err := inApp.HandleInApp(ctx, engine.TaskPayload{RunID: "run-1", StepID: "S2"}); err != nil {
		t.Fatalf("later step: %v", err)
	}
	msgs, err := fx.st.Messages().List(ctx, fx.scope, "user_1", "in_app")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d in-app messages, want 2: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Status == "error" || m.NotificationID != row.ID {
			t.Fatalf("message %+v: want delivered under notification %s", m, row.ID)
		}
	}

	fx.eng.setFail(nil)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("retried Ingest: %v", err)
	}
	if n := fx.eng.runCount(); n != 1 {
		t.Fatalf("runs after retry = %d, want 1", n)
	}
	row = fx.onlyRow(t)
	if row.RunID != "run-1" || row.Status != domain.NotificationStarted {
		t.Fatalf("row after retry = %+v, want run-1, started", row)
	}
}

// TestIngest_RetryAfterTimeoutReusesRowAndRun retries before any step ran: the
// row still has no run id, so the retry starts again under the same
// notification, and dagnats's idempotency key answers with the first run.
func TestIngest_RetryAfterTimeoutReusesRowAndRun(t *testing.T) {
	fx := setupFakeEngine(t)
	fx.eng.setFail(timeoutAfterStart)
	if err := fx.ingest(t); err == nil {
		t.Fatal("Ingest: want the start's timeout")
	}
	first := fx.onlyRow(t)

	fx.eng.setFail(nil)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("retried Ingest: %v", err)
	}
	if n := fx.eng.runCount(); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	row := fx.onlyRow(t)
	if row.ID != first.ID || row.RunID != "run-1" || row.Status != domain.NotificationStarted {
		t.Fatalf("row after retry = %+v, want %s with run-1, started", row, first.ID)
	}
	var env runenv.InAppEnvelope
	if err := json.Unmarshal(fx.eng.input("run-1"), &env); err != nil {
		t.Fatalf("decode run input: %v", err)
	}
	if env.NotificationID != first.ID {
		t.Fatalf("run's envelope names %q, want the row %q", env.NotificationID, first.ID)
	}
}

// TestIngest_RetryAfterSuccessStartsNothing proves a retry of a trigger that
// already succeeded asks the engine for nothing and writes no second row.
func TestIngest_RetryAfterSuccessStartsNothing(t *testing.T) {
	fx := setupFakeEngine(t)
	for i := range 2 {
		if err := fx.ingest(t); err != nil {
			t.Fatalf("Ingest[%d]: %v", i, err)
		}
	}
	if fx.eng.starts != 1 {
		t.Fatalf("StartRun calls = %d, want 1", fx.eng.starts)
	}
	if row := fx.onlyRow(t); row.RunID != "run-1" {
		t.Fatalf("row = %+v, want run-1", row)
	}
}

// TestIngest_RefusedStartMarksFailed: a start the engine refused leaves no
// run, so the row is marked failed rather than left looking in flight. A later
// retry that succeeds reuses the row.
func TestIngest_RefusedStartMarksFailed(t *testing.T) {
	fx := setupFakeEngine(t)
	refused := fmt.Errorf("%w: engine: workflow not found", engine.ErrStartRefused)
	fx.eng.setFail(func(started bool) error {
		if !started {
			return refused
		}
		return nil
	})
	if err := fx.ingest(t); !errors.Is(err, engine.ErrStartRefused) {
		t.Fatalf("Ingest err = %v, want the refusal", err)
	}
	if n := fx.eng.runCount(); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
	row := fx.onlyRow(t)
	if row.Status != domain.NotificationFailed || row.RunID != "" {
		t.Fatalf("row = %+v, want failed with no run id", row)
	}

	fx.eng.setFail(nil)
	if err := fx.ingest(t); err != nil {
		t.Fatalf("retried Ingest: %v", err)
	}
	if again := fx.onlyRow(t); again.ID != row.ID || again.Status != domain.NotificationStarted || again.RunID != "run-1" {
		t.Fatalf("row after retry = %+v, want %s started on run-1", again, row.ID)
	}
}
