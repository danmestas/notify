package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/dagtest"
)

// TestTaskOutlastingAckWaitRunsOnce proves the engine keeps a long task's
// claim alive: a handler that runs well past its AckWait is not redelivered,
// so it runs exactly once and never holds a second slot of its lane.
func TestTaskOutlastingAckWaitRunsOnce(t *testing.T) {
	t.Skip("pending dagnats' worker robustness release: the SDK heartbeats a running handler and skips redelivery of a task already running; un-skip when notify repins to it")
	natsURL := dagtest.StartSidecar(t)
	e, err := New(natsURL, withAckWait(2*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	var runs, inFlight, maxInFlight atomic.Int32
	e.Handle("long.wait", func(_ context.Context, _ TaskPayload) (json.RawMessage, error) {
		runs.Add(1)
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(7 * time.Second)
		inFlight.Add(-1)
		return json.RawMessage(`{}`), nil
	})
	e.StartWorkers(context.Background())

	ctx := context.Background()
	if err := e.RegisterWorkflow(ctx, WorkflowDef{
		Name:  "wf-long-wait",
		Steps: []StepDef{{ID: "only", Task: "long.wait", Type: dag.StepTypeNormal}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	id, err := e.StartRun(ctx, "wf-long-wait", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		snap, gerr := e.GetRun(ctx, id)
		if gerr == nil && snap.IsTerminal() {
			if !snap.Succeeded() {
				t.Fatalf("run status = %q, want completed", snap.Status)
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Give a late redelivery time to show itself before counting.
	time.Sleep(3 * time.Second)
	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1: the claim lapsed and was redelivered", got)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("%d copies of the task ran at once, want 1", got)
	}
}

// TestReady_ReportsStoppedWorker proves /readyz's worker branch: once the
// context StartWorkers was given ends, the worker stops claiming and Ready
// says so even though the NATS connection is still up.
func TestReady_ReportsStoppedWorker(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	e, err := New(natsURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	e.Handle("noop", func(context.Context, TaskPayload) (json.RawMessage, error) { return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	e.StartWorkers(ctx)
	if err := e.Ready(); err != nil {
		t.Fatalf("Ready before cancel: %v", err)
	}
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for e.Ready() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := e.Ready(); err == nil || err.Error() != "task worker stopped" {
		t.Fatalf("Ready after cancel = %v, want task worker stopped", err)
	}
	if !e.nc.IsConnected() {
		t.Fatal("the NATS connection closed too; this test needs only the worker stopped")
	}
}
