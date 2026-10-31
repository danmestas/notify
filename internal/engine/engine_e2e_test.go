package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// TestEngine_StartRun_OneStepCompletes is the headline end-to-end test: it
// spawns a real dagnats sidecar, registers a one-step workflow, starts a run,
// and asserts the run drives to terminal "completed" — proving trigger -> a
// one-step DAG -> complete against the real engine.
func TestEngine_StartRun_OneStepCompletes(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	e, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	e.Handle("noop", func(ctx context.Context, task engine.TaskPayload) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.StartWorkers(ctx)

	def := engine.WorkflowDef{
		Name: "foundation-smoke",
		Steps: []engine.StepDef{
			{ID: "noop", Task: "noop", Type: dag.StepTypeNormal},
		},
	}
	if err := e.RegisterWorkflow(ctx, def); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	id, err := e.StartRun(ctx, "foundation-smoke", nil)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if id == "" {
		t.Fatal("StartRun returned empty run id")
	}

	deadline := time.Now().Add(15 * time.Second)
	var snap engine.RunSnapshot
	for time.Now().Before(deadline) {
		// The run snapshot may not be queryable for a brief window right
		// after StartRun returns; tolerate transient errors while polling.
		s, gerr := e.GetRun(ctx, id)
		if gerr == nil {
			snap = s
			if snap.IsTerminal() {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !snap.IsTerminal() {
		t.Fatalf("run %s never reached terminal state; last snapshot: %+v", id, snap)
	}
	if !snap.Succeeded() {
		t.Fatalf("run %s did not succeed; status=%q snapshot=%+v", id, snap.Status, snap)
	}
}
