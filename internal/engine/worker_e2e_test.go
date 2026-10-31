package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// startEngine connects an engine to a fresh in-process dagnats and registers
// its cleanup. Handlers are registered by the caller before StartWorkers.
func startEngine(t *testing.T) (*engine.Engine, string) {
	t.Helper()
	natsURL := dagtest.StartSidecar(t)
	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	return e, natsURL
}

// runOneStep registers a one-step workflow for task and starts a run of it.
func runOneStep(t *testing.T, e *engine.Engine, name, task string, retry *engine.RetryPolicy, input string) string {
	t.Helper()
	ctx := context.Background()
	def := engine.WorkflowDef{
		Name:  name,
		Steps: []engine.StepDef{{ID: "only", Task: task, Type: dag.StepTypeNormal, Retry: retry}},
	}
	if err := e.RegisterWorkflow(ctx, def); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	var in json.RawMessage
	if input != "" {
		in = json.RawMessage(input)
	}
	id, err := e.StartRun(ctx, name, in)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return id
}

// TestWorker_DispatchesTaskPayload proves a task reaches its handler through
// dagnats' worker with the run, step and input intact, and that the handler's
// output completes the run.
func TestWorker_DispatchesTaskPayload(t *testing.T) {
	e, _ := startEngine(t)
	got := make(chan engine.TaskPayload, 1)
	e.Handle("echo", func(_ context.Context, tp engine.TaskPayload) (json.RawMessage, error) {
		got <- tp
		return json.RawMessage(`{"ok":true}`), nil
	})
	e.StartWorkers(context.Background())

	id := runOneStep(t, e, "wf-echo", "echo", nil, `{"hello":"world"}`)
	snap := waitTerminal(t, e, id, 15*time.Second)
	if !snap.Succeeded() {
		t.Fatalf("run status = %q, want completed", snap.Status)
	}
	tp := <-got
	if tp.RunID != id || tp.StepID != "only" || tp.TaskID != id+".only" || tp.Attempt != 1 {
		t.Fatalf("payload = %+v; want run %s, step only, task %s.only, attempt 1", tp, id, id)
	}
	var in map[string]any
	if err := json.Unmarshal(tp.Input, &in); err != nil {
		t.Fatalf("decode input %s: %v", tp.Input, err)
	}
	if in["hello"] != "world" {
		t.Fatalf("input = %s, want it to carry hello=world", tp.Input)
	}
}

// TestWorker_SameTypeRunsInParallel proves one slow task never blocks others of
// its type: a handler that waits for all of its siblings to arrive can only
// return if they are dispatched at the same time.
func TestWorker_SameTypeRunsInParallel(t *testing.T) {
	const n = 6
	e, _ := startEngine(t)
	var (
		arrived atomic.Int32
		all     = make(chan struct{})
		once    sync.Once
	)
	e.Handle("slow", func(ctx context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		if arrived.Add(1) == n {
			once.Do(func() { close(all) })
		}
		select {
		case <-all:
			return json.RawMessage(`{}`), nil
		case <-time.After(20 * time.Second):
			return nil, errors.New("siblings never ran alongside this task")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	e.StartWorkers(context.Background())

	ctx := context.Background()
	if err := e.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  "wf-slow",
		Steps: []engine.StepDef{{ID: "only", Task: "slow", Type: dag.StepTypeNormal}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	ids := make([]string, n)
	for i := range ids {
		id, err := e.StartRun(ctx, "wf-slow", nil)
		if err != nil {
			t.Fatalf("StartRun %d: %v", i, err)
		}
		ids[i] = id
	}
	select {
	case <-all:
	case <-time.After(15 * time.Second):
		t.Fatalf("only %d of %d same-type tasks ran at once", arrived.Load(), n)
	}
	for _, id := range ids {
		if snap := waitTerminal(t, e, id, 15*time.Second); !snap.Succeeded() {
			t.Fatalf("run %s status = %q, want completed", id, snap.Status)
		}
	}
}

// TestWorker_RetryableErrorRetries proves a RetryableError hands the step back
// to the engine's retry policy, and the retry carries the next attempt number.
func TestWorker_RetryableErrorRetries(t *testing.T) {
	e, _ := startEngine(t)
	var (
		mu       sync.Mutex
		attempts []int
	)
	e.Handle("flaky", func(_ context.Context, tp engine.TaskPayload) (json.RawMessage, error) {
		mu.Lock()
		attempts = append(attempts, tp.Attempt)
		first := len(attempts) == 1
		mu.Unlock()
		if first {
			return nil, engine.NewRetryableError(errors.New("transient"))
		}
		return json.RawMessage(`{}`), nil
	})
	e.StartWorkers(context.Background())

	retry := &engine.RetryPolicy{MaxAttempts: 3, Strategy: dag.RetryFixed, InitialDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	id := runOneStep(t, e, "wf-flaky", "flaky", retry, "")
	if snap := waitTerminal(t, e, id, 20*time.Second); !snap.Succeeded() {
		t.Fatalf("run status = %q, want completed after a retry", snap.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts = %v, want [1 2]", attempts)
	}
}

// TestWorker_PlainErrorDeadLetters proves an unmarked error is permanent: the
// step is not retried even with a retry policy, the run fails, and the task
// lands in dagnats' dead-letter stream.
func TestWorker_PlainErrorDeadLetters(t *testing.T) {
	e, natsURL := startEngine(t)
	var calls atomic.Int32
	e.Handle("broken", func(_ context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("permanent")
	})
	e.StartWorkers(context.Background())

	retry := &engine.RetryPolicy{MaxAttempts: 3, Strategy: dag.RetryFixed, InitialDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	id := runOneStep(t, e, "wf-broken", "broken", retry, "")
	if snap := waitTerminal(t, e, id, 15*time.Second); snap.Status != "failed" {
		t.Fatalf("run status = %q, want failed", snap.Status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1 (a plain error must not retry)", got)
	}
	waitDeadLetter(t, natsURL, id)
}

// TestWorker_RetriesExhaustedDeadLetter proves a step that keeps failing
// retriably is dead-lettered once its policy runs out.
func TestWorker_RetriesExhaustedDeadLetter(t *testing.T) {
	e, natsURL := startEngine(t)
	var calls atomic.Int32
	e.Handle("always-flaky", func(_ context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		calls.Add(1)
		return nil, engine.NewRetryableError(errors.New("still down"))
	})
	e.StartWorkers(context.Background())

	retry := &engine.RetryPolicy{MaxAttempts: 2, Strategy: dag.RetryFixed, InitialDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	id := runOneStep(t, e, "wf-always-flaky", "always-flaky", retry, "")
	if snap := waitTerminal(t, e, id, 20*time.Second); snap.Status != "failed" {
		t.Fatalf("run status = %q, want failed", snap.Status)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("handler ran %d times, want the step retried before dead-lettering", got)
	}
	waitDeadLetter(t, natsURL, id)
}

// TestWorker_PanicFailsOnlyItsStep proves a panicking handler fails its own
// step without taking the worker down: a sibling step of the same run still
// completes, and the same task type keeps serving later runs.
func TestWorker_PanicFailsOnlyItsStep(t *testing.T) {
	e, _ := startEngine(t)
	var siblingRan atomic.Bool
	e.Handle("boom", func(_ context.Context, tp engine.TaskPayload) (json.RawMessage, error) {
		if string(tp.Input) != "" && bytes.Contains(tp.Input, []byte(`"calm"`)) {
			return json.RawMessage(`{}`), nil
		}
		panic("handler blew up")
	})
	e.Handle("sibling", func(_ context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		siblingRan.Store(true)
		return json.RawMessage(`{}`), nil
	})
	e.StartWorkers(context.Background())

	ctx := context.Background()
	if err := e.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name: "wf-boom",
		Steps: []engine.StepDef{
			{ID: "boom", Task: "boom", Type: dag.StepTypeNormal},
			{ID: "sibling", Task: "sibling", Type: dag.StepTypeNormal},
		},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	id, err := e.StartRun(ctx, "wf-boom", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if snap := waitTerminal(t, e, id, 15*time.Second); snap.Status != "failed" {
		t.Fatalf("run status = %q, want failed", snap.Status)
	}
	if !siblingRan.Load() {
		t.Fatal("the sibling step never ran; the panic took more than its own step")
	}

	id2 := runOneStep(t, e, "wf-boom-calm", "boom", nil, `{"mood":"calm"}`)
	if snap := waitTerminal(t, e, id2, 15*time.Second); !snap.Succeeded() {
		t.Fatalf("follow-up run status = %q; the panic left task type boom unserved", snap.Status)
	}
}

// TestWorker_StopDrainsInFlight proves Stop waits for a task already running,
// and that its result is published before the connection closes.
func TestWorker_StopDrainsInFlight(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	started := make(chan struct{})
	var finished atomic.Bool
	e.Handle("steady", func(_ context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		close(started)
		time.Sleep(500 * time.Millisecond)
		finished.Store(true)
		return json.RawMessage(`{"drained":true}`), nil
	})
	e.StartWorkers(context.Background())
	id := runOneStep(t, e, "wf-steady", "steady", nil, "")

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("task never started")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Stop returned while the task was still running")
	}
	again := time.Now()
	if err := e.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if took := time.Since(again); took > time.Second {
		t.Fatalf("second Stop took %v; it must not drain again", took)
	}

	reader, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New (reader): %v", err)
	}
	defer reader.Stop()
	if snap := waitTerminal(t, reader, id, 15*time.Second); !snap.Succeeded() {
		t.Fatalf("drained run status = %q, want completed", snap.Status)
	}
}

// TestWorker_StopCancelsWaitingHandlers proves Stop ends a handler that waits
// on its context, so a long wait never holds shutdown for the drain timeout.
func TestWorker_StopCancelsWaitingHandlers(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	started := make(chan struct{})
	e.Handle("waiter", func(ctx context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, engine.NewRetryableError(ctx.Err())
	})
	e.StartWorkers(context.Background())
	runOneStep(t, e, "wf-waiter", "waiter", nil, "")
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("task never started")
	}
	begin := time.Now()
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("Stop took %v; a waiting handler must see its context end", took)
	}
}

// TestAgentLoop_LargeState proves agent-loop state travels in the task payload:
// about 300 KiB checkpointed on one iteration comes back intact on the next.
func TestAgentLoop_LargeState(t *testing.T) {
	e, _ := startEngine(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16)
	var (
		mu   sync.Mutex
		seen [][]byte
	)
	e.HandleAgentLoop("agent.big", func(_ context.Context, lc *engine.AgentLoopContext) error {
		cp := lc.Checkpoint()
		mu.Lock()
		seen = append(seen, cp)
		mu.Unlock()
		if cp == nil {
			return lc.Continue(big)
		}
		return lc.Complete(json.RawMessage(fmt.Sprintf(`{"bytes":%d}`, len(cp))))
	})
	e.StartWorkers(context.Background())

	ctx := context.Background()
	if err := e.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  "wf-big-state",
		Steps: []engine.StepDef{engine.AgentLoopStep("big", "agent.big", engine.AgentLoopConfig{MaxIterations: 5}, 0)},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	id, err := e.StartRun(ctx, "wf-big-state", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if snap := waitTerminal(t, e, id, 30*time.Second); !snap.Succeeded() {
		t.Fatalf("run status = %q, want completed", snap.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != nil || !bytes.Equal(seen[1], big) {
		t.Fatalf("checkpoints seen: %d iterations, second %d bytes; want nil then %d bytes intact", len(seen), lenAt(seen, 1), len(big))
	}
}

// TestAgentLoop_PanicFailsStep proves a panicking agent-loop handler fails its
// step instead of leaving the run in flight.
func TestAgentLoop_PanicFailsStep(t *testing.T) {
	e, _ := startEngine(t)
	e.HandleAgentLoop("agent.boom", func(_ context.Context, _ *engine.AgentLoopContext) error {
		panic("loop blew up")
	})
	e.StartWorkers(context.Background())

	ctx := context.Background()
	if err := e.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  "wf-loop-boom",
		Steps: []engine.StepDef{engine.AgentLoopStep("boom", "agent.boom", engine.AgentLoopConfig{MaxIterations: 3}, 0)},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	id, err := e.StartRun(ctx, "wf-loop-boom", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if snap := waitTerminal(t, e, id, 15*time.Second); snap.Status != "failed" {
		t.Fatalf("run status = %q, want failed", snap.Status)
	}
}

func lenAt(s [][]byte, i int) int {
	if i < len(s) {
		return len(s[i])
	}
	return -1
}

// waitDeadLetter waits for a DEAD_LETTERS message that names runID.
func waitDeadLetter(t *testing.T, natsURL, runID string) {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cons, err := js.OrderedConsumer(ctx, "DEAD_LETTERS", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatalf("dead-letter consumer: %v", err)
	}
	for ctx.Err() == nil {
		msg, err := cons.Next(jetstream.FetchMaxWait(time.Second))
		if err != nil {
			continue
		}
		if bytes.Contains(msg.Data(), []byte(runID)) || bytes.Contains([]byte(msg.Subject()), []byte(runID)) {
			return
		}
	}
	t.Fatalf("no dead letter for run %s", runID)
}
