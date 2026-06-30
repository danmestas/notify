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
	draft := &WorkflowDraft{Name: "Delay", TriggerIdentifier: "delay"}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		case 2:
			sid := draft.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"type":"regular","amount":5,"unit":"fortnights"}}`, sid)),
			}}
		case 3:
			// Model thinks it's done, but the bad unit fails Compile.
			return MessageResponse{StopReason: "end_turn", Text: "Done."}
		case 4:
			// Self-correct round: fix the unit.
			sid := draft.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t3", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"type":"regular","amount":5,"unit":"minutes"}}`, sid)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Fixed."}
		}
	}}

	b := NewBuilder(client, emailActive())
	var events []Event
	if err := b.Run(context.Background(), domain.Scope{EnvID: "env1"}, draft, "delay 5 fortnights", collect(&events)); err != nil {
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
	draft := &WorkflowDraft{Name: "Delay", TriggerIdentifier: "delay"}
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStep, `{"type":"delay"}`),
			}}
		case 2:
			sid := draft.Steps[0].StepID
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"type":"regular","amount":0,"unit":"minutes"}}`, sid)),
			}}
		default:
			// Never fixes; stops twice with the invalid amount.
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
