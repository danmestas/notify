package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// stubLister is a fixed-list IntegrationLister.
type stubLister struct{ ints []domain.Integration }

func (s stubLister) List(context.Context, domain.Scope) ([]domain.Integration, error) {
	return s.ints, nil
}

// fnClient is a scripted Client: fn maps the 1-based call number to a response
// and may close over the test's draft to reference runtime-generated step ids.
type fnClient struct {
	fn   func(call int, req MessageRequest) MessageResponse
	n    int
	reqs []MessageRequest
}

func (c *fnClient) Message(_ context.Context, req MessageRequest) (MessageResponse, error) {
	c.reqs = append(c.reqs, req)
	c.n++
	return c.fn(c.n, req), nil
}

func collect(events *[]Event) func(Event) {
	return func(e Event) { *events = append(*events, e) }
}

func emailActive() IntegrationLister {
	return stubLister{ints: []domain.Integration{{Channel: "email", Active: true}}}
}

func tc(id, name, input string) ToolCall {
	return ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

func TestRun_AddEmailStep_ThenEditThenStop_Compiles(t *testing.T) {
	draft := &WorkflowDraft{Name: "Welcome", TriggerIdentifier: "welcome"}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"email","name":"Welcome"}`),
			}}
		case 2:
			sid := draft.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"subject":"Hi","body":"Welcome aboard"}}`, sid)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Built a one-step welcome email workflow."}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "welcome new users by email", collect(&events)); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(draft.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(draft.Steps))
	}
	s := draft.Steps[0]
	if s.Type != "email" {
		t.Errorf("Type = %q", s.Type)
	}
	var cv map[string]string
	_ = json.Unmarshal(s.ControlValues, &cv)
	if cv["subject"] != "Hi" || cv["body"] != "Welcome aboard" {
		t.Errorf("controlValues = %v", cv)
	}

	// The resulting draft must independently survive the pure authoring compile.
	stepsJSON, _ := json.Marshal(draft.Steps)
	if _, err := authoring.Compile(domain.Workflow{EnvID: "env1", TriggerIdentifier: "welcome", Steps: stepsJSON}); err != nil {
		t.Errorf("authoring.Compile rejected the built draft: %v", err)
	}

	// A terminal done event with the summary is emitted.
	last := events[len(events)-1]
	if last.Kind != KindDone || !strings.Contains(last.Text, "welcome email") {
		t.Errorf("last event = %+v, want done with summary", last)
	}
}

// stepIDFromToolResult extracts the "stepId" a mutating tool_result surfaced for
// the given tool-call id from the latest user turn in req. It fails the test if
// no such result is present or it carries no stepId — the whole point is that a
// real model learns the id of a step it just added from the tool_result alone,
// not from any out-of-band draft snapshot.
func stepIDFromToolResult(t *testing.T, req MessageRequest, callID string) string {
	t.Helper()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, r := range req.Messages[i].ToolResults {
			if r.ToolCallID != callID {
				continue
			}
			var payload struct {
				StepID string `json:"stepId"`
			}
			if err := json.Unmarshal([]byte(r.Content), &payload); err != nil {
				t.Fatalf("tool_result for %q is not JSON: %q (%v)", callID, r.Content, err)
			}
			if payload.StepID == "" {
				t.Fatalf("tool_result for %q surfaced no stepId: %q", callID, r.Content)
			}
			return payload.StepID
		}
	}
	t.Fatalf("no tool_result found for call id %q", callID)
	return ""
}

// TestRun_AddStep_SurfacesStepIDForEdit proves the id of a freshly added step is
// surfaced to the model in the addStep tool_result: the scripted client reads
// the stepId FROM THE TOOL_RESULT (never from the draft) and targets it with
// editStepContent. This is the non-cheating mirror of the checkpoint-fed test
// above and is the behavior a real LLM depends on.
func TestRun_AddStep_SurfacesStepIDForEdit(t *testing.T) {
	draft := &WorkflowDraft{Name: "Welcome", TriggerIdentifier: "welcome"}
	client := &fnClient{fn: func(call int, req MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"email","name":"Welcome"}`),
			}}
		case 2:
			sid := stepIDFromToolResult(t, req, "t1")
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"subject":"Hi","body":"Welcome aboard"}}`, sid)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Built a one-step welcome email workflow."}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "welcome new users by email", collect(&events)); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(draft.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(draft.Steps))
	}
	var cv map[string]string
	_ = json.Unmarshal(draft.Steps[0].ControlValues, &cv)
	if cv["subject"] != "Hi" || cv["body"] != "Welcome aboard" {
		t.Errorf("controlValues = %v, want subject/body set via the surfaced id", cv)
	}
	if last := events[len(events)-1]; last.Kind != KindDone {
		t.Errorf("last event = %+v, want done", last)
	}
}

func TestRun_EmitsReasoningToolCanvasDoneInOrder(t *testing.T) {
	draft := &WorkflowDraft{Name: "Welcome", TriggerIdentifier: "welcome"}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{
				StopReason: "tool_use", Text: "I will add an email step.",
				ToolCalls: []ToolCall{tc("t1", toolAddStep, `{"type":"email"}`)},
			}
		default:
			return MessageResponse{StopReason: "end_turn"}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "add email", collect(&events)); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var kinds []EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []EventKind{KindReasoning, KindTool, KindCanvas, KindDone}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", kinds, want)
		}
	}
}

func TestRun_SelfCorrectsOnce_OnCompileFailure(t *testing.T) {
	// The injected flaw is structural (no name/trigger set) rather than a bad
	// per-step value: addStep/editStepContent now front-load steptype.Validate
	// (see draft.go), so a bad step value would be rejected as an IsError
	// tool_result on the SAME call instead of riding to the turn-end Compile.
	// The turn-end validateDraft/Compile gate still guards structural shape
	// (name/trigger/empty-steps) — that's what this test exercises.
	draft := &WorkflowDraft{}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		case 2:
			// Model thinks it's done, but the workflow has no name/trigger yet.
			return MessageResponse{StopReason: "end_turn", Text: "Done."}
		case 3:
			// Self-correct round: set the missing metadata.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolSetWorkflowMetadata, `{"name":"Delay"}`),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Fixed."}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "delay 5 minutes", collect(&events)); err != nil {
		t.Fatalf("Run error after self-correct: %v", err)
	}

	// Exactly one validation-failure message was fed back into the conversation.
	feedback := 0
	for _, req := range client.reqs {
		for _, m := range req.Messages {
			if m.Role == RoleUser && strings.Contains(m.Text, "failed validation") {
				feedback++
			}
		}
	}
	// The same conversation grows across calls, so the feedback message appears
	// in every request from the self-correct round onward; assert it was emitted
	// once (present) and the run ultimately succeeded.
	if feedback == 0 {
		t.Error("expected the compile error to be fed back to the model")
	}
	if events[len(events)-1].Kind != KindDone {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}
}

func TestRun_SelfCorrectFailsTwice_EmitsError(t *testing.T) {
	// As above: the injected flaw is structural (name never set) so it survives
	// the new front-loaded per-tool Validate and still reaches the turn-end
	// validateDraft gate, which the synchronous Builder only forgives once.
	draft := &WorkflowDraft{}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		default:
			// Never sets a name; stops twice with the workflow still unnamed.
			return MessageResponse{StopReason: "end_turn", Text: "Done."}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "bad delay", collect(&events))
	if err == nil {
		t.Fatal("expected error when validation still fails after self-correct")
	}
	if events[len(events)-1].Kind != KindError {
		t.Errorf("last event = %+v, want error", events[len(events)-1])
	}
}

func TestRun_RejectsUnconfiguredChannel(t *testing.T) {
	// A pre-existing valid step keeps the workflow non-empty so the rejected
	// sms add is the only behavior under test (an empty workflow would itself
	// fail validation).
	draft := &WorkflowDraft{
		Name:              "Notify",
		TriggerIdentifier: "notify",
		Steps: []domain.Step{{
			StepID:        "pre1",
			Type:          "delay",
			ControlValues: steptype.Default().Get("delay").DefaultControlValues(),
		}},
	}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			// Email is the only active channel; sms must be rejected at runtime.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"sms"}`),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "stopped"}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "text the user", collect(&events)); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(draft.Steps) != 1 {
		t.Fatalf("sms step should have been rejected, draft should still hold only the pre-existing step, got %d steps", len(draft.Steps))
	}
	// The rejection was fed back as an error tool result.
	var sawErrResult bool
	for _, req := range client.reqs {
		for _, m := range req.Messages {
			for _, r := range m.ToolResults {
				if r.IsError && strings.Contains(r.Content, "not available") {
					sawErrResult = true
				}
			}
		}
	}
	if !sawErrResult {
		t.Error("expected an error tool result for the unconfigured channel")
	}
	// No canvas event since nothing mutated.
	for _, e := range events {
		if e.Kind == KindCanvas {
			t.Errorf("unexpected canvas event for rejected step: %+v", e)
		}
	}
}

func TestRun_ActionStepsAlwaysAvailable(t *testing.T) {
	draft := &WorkflowDraft{Name: "Wait", TriggerIdentifier: "wait"}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "added a delay"}
		}
	}}

	// No integrations configured at all.
	b := NewBuilder(client, stubLister{})
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "wait then notify", collect(&events)); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if len(draft.Steps) != 1 || draft.Steps[0].Type != "delay" {
		t.Fatalf("expected one delay step, got %+v", draft.Steps)
	}
}

func TestRun_ExhaustsMaxRounds_EmitsError(t *testing.T) {
	draft := &WorkflowDraft{Name: "Loop", TriggerIdentifier: "loop"}
	// A model that never stops calling tools: every turn returns a (harmless)
	// reasoning call, so the loop only ends by exhausting its round budget.
	client := &fnClient{fn: func(int, MessageRequest) MessageResponse {
		return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
			tc("t", toolReasoning, `{"label":"thinking","thought":"still going"}`),
		}}
	}}

	b := NewBuilder(client, emailActive(), WithMaxRounds(3))
	var events []Event
	err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "never stop", collect(&events))
	if err == nil {
		t.Fatal("expected an error when the round budget is exhausted")
	}
	if !strings.Contains(err.Error(), "max rounds") {
		t.Errorf("err = %v, want a max-rounds error", err)
	}
	if client.n != 3 {
		t.Errorf("model called %d times, want 3 (maxRounds)", client.n)
	}
	if last := events[len(events)-1]; last.Kind != KindError {
		t.Errorf("last event = %+v, want a terminal error event", last)
	}
}

func TestRun_RejectsStepBeyondCap(t *testing.T) {
	draft := &WorkflowDraft{Name: "Capped", TriggerIdentifier: "capped"}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		case 2:
			// Cap is 1; this second add must be rejected (fed back, not fatal).
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolAddStep, `{"type":"delay"}`),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "done"}
		}
	}}

	b := NewBuilder(client, emailActive(), WithMaxSteps(1))
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "add too many", collect(&events)); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(draft.Steps) != 1 {
		t.Fatalf("step cap not enforced: got %d steps, want 1", len(draft.Steps))
	}
	var sawCapError bool
	for _, req := range client.reqs {
		for _, m := range req.Messages {
			for _, r := range m.ToolResults {
				if r.IsError && strings.Contains(r.Content, "step limit") {
					sawCapError = true
				}
			}
		}
	}
	if !sawCapError {
		t.Error("expected an error tool result when exceeding the step cap")
	}
	if events[len(events)-1].Kind != KindDone {
		t.Errorf("last event = %+v, want done (the cap rejection is non-fatal)", events[len(events)-1])
	}
}
