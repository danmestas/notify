package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// TestEngine_AgentLoop_CheckpointRoundTrip is the headline durable-loop test.
// It registers a one-step agent-loop workflow whose handler loads an int
// counter from the checkpoint (0 when absent), increments it, and Continues
// with the new checkpoint until the counter hits 3, then Completes. It proves,
// against a real dagnats worker, that: the handler is invoked across multiple
// iterations, the checkpoint round-trips (each call sees the counter 0,1,2), and the final Complete output carries the total.
func TestEngine_AgentLoop_CheckpointRoundTrip(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	var (
		mu          sync.Mutex
		iters       []int
		completeOut json.RawMessage
	)
	e.HandleAgentLoop("agent.count", func(_ context.Context, lc *engine.AgentLoopContext) error {
		n := 0
		if cp := lc.Checkpoint(); cp != nil {
			var c struct {
				N int `json:"n"`
			}
			if uErr := json.Unmarshal(cp, &c); uErr != nil {
				return uErr
			}
			n = c.N
		}
		mu.Lock()
		iters = append(iters, n)
		mu.Unlock()
		n++
		if n >= 3 {
			out := json.RawMessage(fmt.Sprintf(`{"total":%d}`, n))
			mu.Lock()
			completeOut = out
			mu.Unlock()
			return lc.Complete(out)
		}
		return lc.Continue(json.RawMessage(fmt.Sprintf(`{"n":%d}`, n)))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	def := engine.WorkflowDef{
		Name: "agent-loop-counter",
		Steps: []engine.StepDef{
			engine.AgentLoopStep("count", "agent.count", engine.AgentLoopConfig{MaxIterations: 10}, 0),
		},
	}
	if err := e.RegisterWorkflow(ctx, def); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	id, err := e.StartRun(ctx, "agent-loop-counter", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	snap := waitTerminal(t, e, id, 30*time.Second)
	if !snap.Succeeded() {
		t.Fatalf("run %s did not succeed; status=%q", id, snap.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(iters) != 3 {
		t.Fatalf("handler invoked %d times, want 3; iters=%v", len(iters), iters)
	}
	for i, got := range iters {
		if got != i {
			t.Fatalf("checkpoint at call %d = %d, want %d (iters=%v)", i, got, i, iters)
		}
	}
	if string(completeOut) != `{"total":3}` {
		t.Fatalf("final Complete output = %s, want {\"total\":3}", completeOut)
	}
}

// TestEngine_AgentLoop_MaxIterationsEnforced asserts that an agent-loop handler
// that never Completes is bounded: the engine fails the run with the
// iteration-limit error once Iterations reaches MaxIterations.
func TestEngine_AgentLoop_MaxIterationsEnforced(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	var (
		mu    sync.Mutex
		calls int
	)
	e.HandleAgentLoop("agent.forever", func(_ context.Context, lc *engine.AgentLoopContext) error {
		mu.Lock()
		calls++
		mu.Unlock()
		// Never Complete: always ask for another iteration.
		return lc.Continue(nil)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	def := engine.WorkflowDef{
		Name: "agent-loop-forever",
		Steps: []engine.StepDef{
			engine.AgentLoopStep("loop", "agent.forever", engine.AgentLoopConfig{MaxIterations: 3}, 0),
		},
	}
	if err := e.RegisterWorkflow(ctx, def); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	id, err := e.StartRun(ctx, "agent-loop-forever", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	snap := waitTerminal(t, e, id, 30*time.Second)
	if snap.Succeeded() {
		t.Fatalf("run %s unexpectedly succeeded; a never-completing loop must fail", id)
	}
	if snap.Status != "failed" {
		t.Fatalf("run %s terminal status = %q, want failed", id, snap.Status)
	}
	// MaxIterations=3 admits iterations 0,1,2 (3 dispatches); the 3rd Continue
	// drives Iterations to 3 and trips the bound.
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("handler invoked %d times, want 3 before the iteration bound trips", calls)
	}
}

// TestEngine_AgentLoop_Stream proves the streaming surface: a handler Streams
// raw bytes that a plain NATS subscriber on stream.{runID}.{stepID} receives,
// then Completes the run.
func TestEngine_AgentLoop_Stream(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	// Subscribe before the run exists so no stream token can be lost to a race.
	sc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("subscriber nats.Connect: %v", err)
	}
	defer sc.Close()
	sub, err := sc.SubscribeSync("stream.>")
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	if err := sc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	const want = "tick-tock"
	e.HandleAgentLoop("agent.stream", func(_ context.Context, lc *engine.AgentLoopContext) error {
		if sErr := lc.Stream([]byte(want)); sErr != nil {
			return sErr
		}
		return lc.Complete(json.RawMessage(`{"streamed":true}`))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	def := engine.WorkflowDef{
		Name: "agent-loop-stream",
		Steps: []engine.StepDef{
			engine.AgentLoopStep("emit", "agent.stream", engine.AgentLoopConfig{MaxIterations: 5}, 0),
		},
	}
	if err := e.RegisterWorkflow(ctx, def); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	id, err := e.StartRun(ctx, "agent-loop-stream", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	msg, err := sub.NextMsg(15 * time.Second)
	if err != nil {
		t.Fatalf("did not receive streamed message: %v", err)
	}
	if string(msg.Data) != want {
		t.Fatalf("streamed payload = %q, want %q", msg.Data, want)
	}
	wantSubject := "stream." + id + ".emit"
	if msg.Subject != wantSubject {
		t.Fatalf("streamed subject = %q, want %q", msg.Subject, wantSubject)
	}

	snap := waitTerminal(t, e, id, 30*time.Second)
	if !snap.Succeeded() {
		t.Fatalf("run %s did not succeed; status=%q", id, snap.Status)
	}
}

// waitTerminal polls GetRun until the run reaches a terminal state or the
// deadline elapses, tolerating transient lookup errors right after StartRun.
func waitTerminal(t *testing.T, e *engine.Engine, id string, within time.Duration) engine.RunSnapshot {
	t.Helper()
	deadline := time.Now().Add(within)
	var snap engine.RunSnapshot
	for time.Now().Before(deadline) {
		s, gerr := e.GetRun(context.Background(), id)
		if gerr == nil {
			snap = s
			if snap.IsTerminal() {
				return snap
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s never reached terminal state; last snapshot: %+v", id, snap)
	return snap
}
