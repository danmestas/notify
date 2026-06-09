package engine_test

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// TestEngine_RegisterWorkflow_BadDef_Errors asserts the {"error":...} reply
// path. dagnats's handleRegister -> Service.RegisterWorkflow -> dag.Validate
// rejects a step that DependsOn a non-existent step id ("step %q depends on %q
// which does not exist"), so RegisterWorkflow must surface a non-nil error.
// (An empty-Name def is NOT used: the server panics on it rather than replying
// with an error envelope.)
func TestEngine_RegisterWorkflow_BadDef_Errors(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)

	e, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	bad := engine.WorkflowDef{
		Name: "bad-dep",
		Steps: []engine.StepDef{
			{ID: "a", Task: "noop", Type: "normal", DependsOn: []string{"nonexistent"}},
		},
	}
	if err := e.RegisterWorkflow(context.Background(), bad); err == nil {
		t.Fatal("expected error registering workflow with missing dependency, got nil")
	}
}

// TestEngine_GetRun_UnknownID_Errors asserts GetRun returns an error (not a
// zero snapshot with empty status) for an unknown run id.
func TestEngine_GetRun_UnknownID_Errors(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)

	e, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	snap, err := e.GetRun(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatalf("expected error for unknown run id, got nil (snapshot=%+v)", snap)
	}
}
