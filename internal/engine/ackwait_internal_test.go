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

// parkingHandler waits until a deadline by parking: the first dispatch parks
// until now+wait, and the resumed one completes. It counts both.
type parkingHandler struct {
	wait      time.Duration
	parks     atomic.Int32
	completes atomic.Int32
	resumedAt atomic.Value // time.Time: the ResumeAt the resumed dispatch saw
	parkedAt  atomic.Value // time.Time: the deadline the first dispatch asked for
}

func (h *parkingHandler) handle(_ context.Context, t TaskPayload) (json.RawMessage, error) {
	if t.ResumeAt.IsZero() {
		h.parks.Add(1)
		at := time.Now().Add(h.wait)
		h.parkedAt.Store(at)
		return nil, WaitUntil(at)
	}
	if time.Now().Before(t.ResumeAt) {
		return nil, WaitUntil(t.ResumeAt)
	}
	h.resumedAt.Store(t.ResumeAt)
	h.completes.Add(1)
	return json.RawMessage(`{}`), nil
}

func registerOneStep(t *testing.T, e *Engine, name, task string) {
	t.Helper()
	if err := e.RegisterWorkflow(context.Background(), WorkflowDef{
		Name:  name,
		Steps: []StepDef{{ID: "only", Task: task, Type: dag.StepTypeNormal}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
}

func awaitRun(t *testing.T, e *Engine, id string, within time.Duration) RunSnapshot {
	t.Helper()
	deadline := time.Now().Add(within)
	var snap RunSnapshot
	for time.Now().Before(deadline) {
		s, err := e.GetRun(context.Background(), id)
		if err == nil {
			snap = s
			if s.IsTerminal() {
				return s
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s never finished; last %+v", id, snap)
	return snap
}

// TestParkedTaskOutlastsAckWaitWithoutHoldingItsLane proves a wait longer
// than the claim's AckWait parks instead of holding a worker: with one slot
// per lane, a second task of the same type runs and finishes while the first
// is parked, and the parked one then completes exactly once.
func TestParkedTaskOutlastsAckWaitWithoutHoldingItsLane(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	e, err := New(natsURL, withAckWait(2*time.Second), withConcurrency(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })

	h := &parkingHandler{wait: 6 * time.Second}
	var quick atomic.Int32
	e.Handle("park.wait", func(ctx context.Context, tp TaskPayload) (json.RawMessage, error) {
		if string(tp.Input) == `{"quick":true}` {
			quick.Add(1)
			return json.RawMessage(`{}`), nil
		}
		return h.handle(ctx, tp)
	})
	if err := e.StartWorkers(context.Background()); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	registerOneStep(t, e, "wf-park", "park.wait")

	ctx := context.Background()
	parked, err := e.StartRun(ctx, "wf-park", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for h.parks.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	other, err := e.StartRun(ctx, "wf-park", json.RawMessage(`{"quick":true}`))
	if err != nil {
		t.Fatalf("StartRun (quick): %v", err)
	}
	if snap := awaitRun(t, e, other, 4*time.Second); !snap.Succeeded() {
		t.Fatalf("quick run status = %q; the parked task held the lane", snap.Status)
	}
	if h.completes.Load() != 0 {
		t.Fatal("the parked task completed before its deadline")
	}

	if snap := awaitRun(t, e, parked, 30*time.Second); !snap.Succeeded() {
		t.Fatalf("parked run status = %q, want completed", snap.Status)
	}
	time.Sleep(3 * time.Second) // let a stray redelivery show itself
	if got := h.completes.Load(); got != 1 {
		t.Fatalf("parked task completed %d times, want 1", got)
	}
	if got := h.parks.Load(); got != 1 {
		t.Fatalf("parked task started from scratch %d times, want 1", got)
	}
	if want, got := h.parkedAt.Load().(time.Time), h.resumedAt.Load().(time.Time); !got.Equal(want) {
		t.Fatalf("resumed with ResumeAt %v, want the parked deadline %v", got, want)
	}
}

// TestParkedTaskResumesAfterRestart proves a parked task survives notify
// going away mid-wait: a fresh engine on the same dagnats resumes it at the
// original deadline and completes it once.
func TestParkedTaskResumesAfterRestart(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	h := &parkingHandler{wait: 4 * time.Second}

	first, err := New(natsURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first.Handle("park.restart", h.handle)
	if err := first.StartWorkers(context.Background()); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	registerOneStep(t, first, "wf-park-restart", "park.restart")
	id, err := first.StartRun(context.Background(), "wf-park-restart", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for h.parks.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	second, err := New(natsURL)
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	t.Cleanup(func() { _ = second.Stop() })
	second.Handle("park.restart", h.handle)
	if err := second.StartWorkers(context.Background()); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	if snap := awaitRun(t, second, id, 30*time.Second); !snap.Succeeded() {
		t.Fatalf("run status = %q, want completed after the restart", snap.Status)
	}
	if got := h.completes.Load(); got != 1 {
		t.Fatalf("completed %d times, want 1", got)
	}
	if got := h.parks.Load(); got != 1 {
		t.Fatalf("started from scratch %d times, want 1: the restart lost the deadline", got)
	}
}

// runLongTask registers a one-step workflow over a handler that sleeps for
// run on its first call, starts a run of it and waits for the run to finish.
// It returns how many times the handler ran and the most copies of it that
// ran at once.
func runLongTask(t *testing.T, e *Engine, task string, run time.Duration, opts ...HandleOption) (runs, maxInFlight int32) {
	t.Helper()
	var n, inFlight, peak atomic.Int32
	e.Handle(task, func(context.Context, TaskPayload) (json.RawMessage, error) {
		first := n.Add(1) == 1
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		if first {
			time.Sleep(run)
		}
		return json.RawMessage(`{}`), nil
	}, opts...)
	if err := e.StartWorkers(context.Background()); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	registerOneStep(t, e, "wf-"+task, task)
	id, err := e.StartRun(context.Background(), "wf-"+task, nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if snap := awaitRun(t, e, id, 30*time.Second); !snap.Succeeded() {
		t.Fatalf("run status = %q, want completed", snap.Status)
	}
	time.Sleep(3 * time.Second) // let a stray redelivery show itself
	return n.Load(), peak.Load()
}

// TestTaskOutlastingAckWaitRunsOnce proves dagnats' automatic heartbeat keeps
// a running handler's claim: one that runs well past a 2s AckWait is never
// redelivered, so it runs exactly once and never takes a second slot.
func TestTaskOutlastingAckWaitRunsOnce(t *testing.T) {
	e, err := New(dagtest.StartSidecar(t), withAckWait(2*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	runs, peak := runLongTask(t, e, "long.run", 7*time.Second)
	if runs != 1 || peak != 1 {
		t.Fatalf("handler ran %d times, %d at once; want once: the claim lapsed past AckWait", runs, peak)
	}
}

// TestMaxRunTimeReachesTheWorker proves WithMaxRunTime is the heartbeat's cap
// for its type: a handler that outruns a 3s cap stops being kept alive, so
// its task redelivers after the 2s AckWait and runs a second time.
func TestMaxRunTimeReachesTheWorker(t *testing.T) {
	e, err := New(dagtest.StartSidecar(t), withAckWait(2*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	runs, _ := runLongTask(t, e, "capped.run", 9*time.Second, WithMaxRunTime(3*time.Second))
	if runs < 2 {
		t.Fatalf("handler ran %d times, want a redelivery once it outran its 3s max run time", runs)
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
	if err := e.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
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
