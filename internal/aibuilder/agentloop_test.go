package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// errClient is a Client that always fails with the given error.
type errClient struct{ err error }

func (c *errClient) Message(_ context.Context, _ MessageRequest) (MessageResponse, error) {
	return MessageResponse{}, c.err
}

// slogCapture records every slog.Record emitted to it. Implements slog.Handler.
type slogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *slogCapture) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *slogCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *slogCapture) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *slogCapture) WithGroup(_ string) slog.Handler      { return h }

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
	// finish flips the session to the terminal Ready status (both create + refine)
	// so the confirm endpoint only ever confirms a completed draft.
	if sess.Status != store.BuilderSessionReady {
		t.Fatalf("session status = %q, want %q after a successful build", sess.Status, store.BuilderSessionReady)
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
//
// The injected flaw is structural (no name set) rather than a bad per-step
// value: applyToolCall now front-loads steptype.Validate for addStep/
// addStepInBetween/editStepContent (see draft.go), so a bad step value is
// rejected as an IsError tool_result on the SAME call instead of riding to the
// turn-end Compile. The turn-end validateDraft/Compile gate still guards
// structural shape (name/trigger/empty-steps) — that's what this test exercises.
func TestAgentLoop_SelfCorrectRidesTheLoop(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "delay 5 then notify")

	var cur WorkflowDraft
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		case 2:
			// Model thinks it is done, but the workflow has no name/trigger yet.
			return MessageResponse{StopReason: "end_turn", Text: "Done."}
		case 3:
			// Self-correct: set the missing metadata.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t3", toolSetWorkflowMetadata, `{"name":"Delayed"}`),
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

	// The fixed draft persisted with the metadata the self-correct turn set.
	sess, _ := s.BuilderSessions().Get(context.Background(), scope, sid)
	var persisted WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &persisted); err != nil {
		t.Fatalf("decode persisted: %v", err)
	}
	if persisted.Name != "Delayed" {
		t.Fatalf("persisted name = %q, want Delayed", persisted.Name)
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

// TestAgentLoop_MessageErrorLogsAndStreamsReason asserts that when client.Message
// fails, handle (a) streams a KindError event whose Error field carries the actual
// error reason, and (b) logs the failure at slog.LevelError with sessionID and
// iter context attributes so an operator can diagnose the failure from the log.
func TestAgentLoop_MessageErrorLogsAndStreamsReason(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "build something")

	// Capture slog output so we can assert the log entry.
	cap := &slogCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(old) })

	modelErr := fmt.Errorf("openrouter: status 404 model not found")
	b := NewAgentLoopBuilder(nil, &errClient{err: modelErr}, emailActive(), s.BuilderSessions())

	lc := &fakeLoopCtx{iter: 2, input: runInputJSON(t, scope, sid)}
	if err := b.handle(context.Background(), lc); err == nil {
		t.Fatal("expected handle to return an error when Message() fails")
	}
	if lc.completed || lc.continued {
		t.Fatal("handle must not resolve when Message() fails")
	}

	// The KindError stream event must carry the actual error reason.
	if len(lc.streams) == 0 {
		t.Fatal("no stream events emitted on Message() error")
	}
	var ev streamEvent
	if err := json.Unmarshal(lc.streams[0], &ev); err != nil {
		t.Fatalf("decode stream event: %v", err)
	}
	if ev.Kind != KindError {
		t.Errorf("first stream event kind = %q, want %q", ev.Kind, KindError)
	}
	if !strings.Contains(ev.Error, "404") {
		t.Errorf("KindError event Error = %q, want to contain the actual reason", ev.Error)
	}

	// An slog.LevelError record must have been emitted with sessionID and iter.
	cap.mu.Lock()
	records := make([]slog.Record, len(cap.records))
	copy(records, cap.records)
	cap.mu.Unlock()

	var found bool
	for _, r := range records {
		if r.Level != slog.LevelError {
			continue
		}
		var hasSession, hasIter bool
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "sessionID":
				hasSession = true
			case "iter":
				hasIter = true
			}
			return true
		})
		if hasSession && hasIter {
			found = true
			break
		}
	}
	if !found {
		var msgs []string
		for _, r := range records {
			msgs = append(msgs, fmt.Sprintf("[%s] %s", r.Level, r.Message))
		}
		t.Errorf("no slog.Error record with sessionID+iter attrs found; logged: %v", msgs)
	}
}
