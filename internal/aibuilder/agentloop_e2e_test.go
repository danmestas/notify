package aibuilder

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// scriptedClient is a thread-safe scripted Client for the e2e loop: each iteration
// runs the handler in its own dispatch goroutine, so the call counter is guarded.
// The script needs no runtime step ids (it relies on the delay step's defaults
// compiling) so it drives the durable loop without intercepting the checkpoint.
type scriptedClient struct {
	mu sync.Mutex
	n  int
	fn func(call int) MessageResponse
}

func (c *scriptedClient) Message(_ context.Context, _ MessageRequest) (MessageResponse, error) {
	c.mu.Lock()
	c.n++
	call := c.n
	c.mu.Unlock()
	return c.fn(call), nil
}

// TestAgentLoop_E2E_DurableBuildPersistsToSession proves the AgentLoop harness
// end-to-end against a real dagnats sidecar: a seeded session is built by a
// mock-driven loop that iterates over the bridge (setWorkflowMetadata -> addStep
// -> stop), the run reaches a terminal completed state, and the final draft lands
// in builder_sessions. It exercises Register/RegisterWorkflow/Dispatch and the
// checkpoint round-trip across real iterations.
func TestAgentLoop_E2E_DurableBuildPersistsToSession(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer func() { _ = eng.Stop() }()

	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "wait a bit then notify")

	client := &scriptedClient{fn: func(call int) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolSetWorkflowMetadata, `{"name":"E2E Flow"}`),
			}}
		case 2:
			// A delay step's default control values compile, so no edit step (and
			// thus no runtime step id) is needed to reach a valid draft.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolAddStep, `{"type":"delay"}`),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Built a one-step delay workflow."}
		}
	}}

	b := NewAgentLoopBuilder(eng, client, stubLister{}, s.BuilderSessions(), WithAgentLoopMaxIterations(10))
	b.Register()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.StartWorkers(ctx)

	if err := b.RegisterWorkflow(ctx); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	runID, err := b.Dispatch(ctx, scope, sid)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap := waitTerminal(t, eng, runID, 30*time.Second)
	if !snap.Succeeded() {
		t.Fatalf("run %s did not succeed; status=%q", runID, snap.Status)
	}

	// The durable result is the persisted draft (run output is not relied upon).
	sess, err := s.BuilderSessions().Get(context.Background(), scope, sid)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	var draft WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &draft); err != nil {
		t.Fatalf("decode persisted draft: %v", err)
	}
	if draft.Name != "E2E Flow" || draft.TriggerIdentifier != "e2e-flow" {
		t.Fatalf("persisted metadata = %q/%q", draft.Name, draft.TriggerIdentifier)
	}
	if len(draft.Steps) != 1 || draft.Steps[0].Type != "delay" {
		t.Fatalf("persisted steps = %+v, want one delay step", draft.Steps)
	}

	// The loop iterated at least 3 model turns over the bridge (set, add, stop).
	client.mu.Lock()
	calls := client.n
	client.mu.Unlock()
	if calls < 3 {
		t.Fatalf("model called %d times, want >= 3 (proves multi-iteration durable loop)", calls)
	}
}

// waitTerminal polls GetRun until the run reaches a terminal state or the
// deadline elapses, tolerating transient lookup errors right after StartRun.
func waitTerminal(t *testing.T, e *engine.Engine, id string, within time.Duration) engine.RunSnapshot {
	t.Helper()
	deadline := time.Now().Add(within)
	var snap engine.RunSnapshot
	for time.Now().Before(deadline) {
		if s, gerr := e.GetRun(context.Background(), id); gerr == nil {
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
