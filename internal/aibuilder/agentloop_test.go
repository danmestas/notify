package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// fakeLoopCtx is a hand-driven loopContext: it stands in for
// *engine.AgentLoopContext so handle() can be exercised across iterations with
// no live engine or NATS. The test driver feeds each Continue's checkpoint back
// as the next iteration's Checkpoint().
type fakeLoopCtx struct {
	iter       int
	checkpoint []byte
	input      json.RawMessage

	continued      bool
	nextCheckpoint []byte
	completed      bool
	completeOutput json.RawMessage
	streams        [][]byte
}

func (f *fakeLoopCtx) Iteration() int         { return f.iter }
func (f *fakeLoopCtx) Checkpoint() []byte     { return f.checkpoint }
func (f *fakeLoopCtx) Input() json.RawMessage { return f.input }
func (f *fakeLoopCtx) Continue(cp []byte) error {
	f.continued = true
	f.nextCheckpoint = cp
	return nil
}

func (f *fakeLoopCtx) Complete(o json.RawMessage) error {
	f.completed = true
	f.completeOutput = o
	return nil
}

func (f *fakeLoopCtx) Stream(d []byte) error {
	f.streams = append(f.streams, append([]byte(nil), d...))
	return nil
}

func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// driveLoop simulates the engine driving the agent-loop: it calls handle once
// per iteration, threading each Continue's checkpoint into the next iteration's
// Checkpoint(), until handle Completes (or the cap trips). Before every
// iteration it decodes the current checkpoint draft into cur so a scripted
// client can reference runtime-generated step ids the way a real model would
// reference ids surfaced on the canvas.
func driveLoop(t *testing.T, b *AgentLoopBuilder, input json.RawMessage, cur *WorkflowDraft, maxIters int) (*fakeLoopCtx, [][]byte) {
	t.Helper()
	var (
		checkpoint []byte
		streams    [][]byte
	)
	for i := 0; i < maxIters; i++ {
		if checkpoint != nil && cur != nil {
			var st loopState
			if err := json.Unmarshal(checkpoint, &st); err != nil {
				t.Fatalf("decode checkpoint at iter %d: %v", i, err)
			}
			*cur = st.Draft
		}
		lc := &fakeLoopCtx{iter: i, checkpoint: checkpoint, input: input}
		if err := b.handle(context.Background(), lc); err != nil {
			t.Fatalf("handle iter %d: %v", i, err)
		}
		streams = append(streams, lc.streams...)
		if lc.completed {
			return lc, streams
		}
		if !lc.continued {
			t.Fatalf("iter %d neither continued nor completed", i)
		}
		checkpoint = lc.nextCheckpoint
	}
	t.Fatalf("loop did not complete within %d iterations", maxIters)
	return nil, nil
}

func seedSession(t *testing.T, s *store.Store, scope domain.Scope, prompt string) string {
	t.Helper()
	sess, err := s.BuilderSessions().Create(context.Background(), scope, prompt)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return sess.ID
}

func runInputJSON(t *testing.T, scope domain.Scope, sessionID string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(runInput{SessionID: sessionID, OrgID: scope.OrgID, EnvID: scope.EnvID})
	if err != nil {
		t.Fatalf("marshal run input: %v", err)
	}
	return raw
}

// TestAgentLoop_InitsBuildsValidatesPersists is the headline per-iteration test:
// iteration 0 inits from the session prompt; a scripted addStep + editStepContent
// then stop builds, validates, and persists the draft to the session, and the
// loop Completes. It also asserts the {messages,draft} checkpoint round-trips —
// iteration 1's request carries iteration 0's assistant tool_use and tool_result.
func TestAgentLoop_InitsBuildsValidatesPersists(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "welcome new users by email")

	var cur WorkflowDraft
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolSetWorkflowMetadata, `{"name":"Welcome Email"}`),
			}}
		case 2:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolAddStep, `{"type":"email","name":"Welcome"}`),
			}}
		case 3:
			sidStep := cur.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t3", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"subject":"Hi","body":"Welcome aboard"}}`, sidStep)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Built a one-step welcome email workflow."}
		}
	}}

	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())
	lc, streams := driveLoop(t, b, runInputJSON(t, scope, sid), &cur, 10)

	if !lc.completed {
		t.Fatal("loop did not Complete")
	}

	// The draft was persisted to the session (the durable result).
	sess, err := s.BuilderSessions().Get(context.Background(), scope, sid)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	var persisted WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &persisted); err != nil {
		t.Fatalf("decode persisted draft: %v", err)
	}
	if persisted.Name != "Welcome Email" || persisted.TriggerIdentifier != "welcome-email" {
		t.Fatalf("persisted metadata = %q/%q", persisted.Name, persisted.TriggerIdentifier)
	}
	if len(persisted.Steps) != 1 || persisted.Steps[0].Type != "email" {
		t.Fatalf("persisted steps = %+v, want one email step", persisted.Steps)
	}
	var cv map[string]string
	_ = json.Unmarshal(persisted.Steps[0].ControlValues, &cv)
	if cv["subject"] != "Hi" || cv["body"] != "Welcome aboard" {
		t.Fatalf("persisted controlValues = %v", cv)
	}

	// Complete carries the same draft JSON the session holds.
	if string(lc.completeOutput) != string(sess.DraftWorkflow) {
		t.Fatalf("Complete output != persisted draft\n got %s\nwant %s", lc.completeOutput, sess.DraftWorkflow)
	}

	// Checkpoint round-trip: the request for call 3 must carry the assistant
	// tool_use from call 2 and the matching tool_result — proof the conversation
	// survived two Continue/checkpoint hops.
	req3 := client.reqs[2]
	var sawAssistantTool, sawToolResult bool
	for _, m := range req3.Messages {
		if m.Role == RoleAssistant {
			for _, tcl := range m.ToolCalls {
				if tcl.Name == toolAddStep {
					sawAssistantTool = true
				}
			}
		}
		if m.Role == RoleUser {
			for _, tr := range m.ToolResults {
				if tr.ToolCallID == "t2" {
					sawToolResult = true
				}
			}
		}
	}
	if !sawAssistantTool || !sawToolResult {
		t.Fatalf("checkpoint did not round-trip the conversation: assistantTool=%v toolResult=%v", sawAssistantTool, sawToolResult)
	}

	// A terminal done event was streamed.
	var sawDone bool
	for _, raw := range streams {
		var ev streamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode stream event: %v", err)
		}
		if ev.Kind == KindDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("no done event streamed")
	}
}

// TestAgentLoop_SelfCorrectRidesTheLoop proves a compile failure is fed back as a
// user message and the loop Continues (self-correct rides the natural loop), then
// a fix lands and the run Completes — all state carried in the checkpoint.
func TestAgentLoop_SelfCorrectRidesTheLoop(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "delay 5 then notify")

	var cur WorkflowDraft
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolSetWorkflowMetadata, `{"name":"Delayed"}`),
			}}
		case 2:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolAddStep, `{"type":"delay"}`),
			}}
		case 3:
			sidStep := cur.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t3", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"type":"regular","amount":5,"unit":"fortnights"}}`, sidStep)),
			}}
		case 4:
			// Model thinks it is done, but the bad unit fails Compile.
			return MessageResponse{StopReason: "end_turn", Text: "Done."}
		case 5:
			// Self-correct: fix the unit.
			sidStep := cur.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t5", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"type":"regular","amount":5,"unit":"minutes"}}`, sidStep)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Fixed."}
		}
	}}

	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())
	lc, _ := driveLoop(t, b, runInputJSON(t, scope, sid), &cur, 10)

	if !lc.completed {
		t.Fatal("self-correcting loop did not Complete")
	}

	// The validation error was fed back as a user message and survived the
	// checkpoint round-trip into a later request (the self-correct turn).
	var sawFeedback bool
	for _, req := range client.reqs {
		for _, m := range req.Messages {
			if m.Role == RoleUser && strings.Contains(m.Text, "failed validation") {
				sawFeedback = true
			}
		}
	}
	if !sawFeedback {
		t.Fatal("compile error was not fed back to the model across the loop")
	}

	// The fixed draft persisted with the corrected unit.
	sess, _ := s.BuilderSessions().Get(context.Background(), scope, sid)
	var persisted WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &persisted); err != nil {
		t.Fatalf("decode persisted: %v", err)
	}
	var cv map[string]any
	_ = json.Unmarshal(persisted.Steps[0].ControlValues, &cv)
	if cv["unit"] != "minutes" {
		t.Fatalf("persisted unit = %v, want minutes", cv["unit"])
	}
}

// TestAgentLoop_LoadStateFailsForMissingSession asserts iteration 0 surfaces a
// load error (which the engine maps to a failed task) when the session id is
// unknown in scope.
func TestAgentLoop_LoadStateFailsForMissingSession(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}

	client := &fnClient{fn: func(int, MessageRequest) MessageResponse {
		return MessageResponse{StopReason: "end_turn"}
	}}
	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())

	lc := &fakeLoopCtx{iter: 0, input: runInputJSON(t, scope, "does-not-exist")}
	if err := b.handle(context.Background(), lc); err == nil {
		t.Fatal("expected handle to error for an unknown session")
	}
	if lc.completed || lc.continued {
		t.Fatal("handle must not resolve when state load fails")
	}
	if client.n != 0 {
		t.Fatalf("model called %d times, want 0 (state load fails before the model)", client.n)
	}
}
