package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestAgentLoop_FollowUpTurnSeedsCurrentDraftAndAccumulates proves the
// generalized loadState: a create session's FIRST turn builds from an empty draft,
// then a follow-up turn (a new run over the SAME session, driven by a chat message
// appended to the session) seeds from the now-non-empty draft and ACCUMULATES —
// the turn-1 email step survives and the turn-2 sms step is added on top. It also
// proves the follow-up turn uses the refine (edit) system prompt and is driven by
// the latest chat message, not the original prompt.
func TestAgentLoop_FollowUpTurnSeedsCurrentDraftAndAccumulates(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "welcome new users by email")

	lister := stubLister{ints: []domain.Integration{
		{Channel: "email", Active: true},
		{Channel: "sms", Active: true},
	}}

	// --- Turn 1: build a one-step email workflow from empty. ---
	var cur1 WorkflowDraft
	client1 := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolSetWorkflowMetadata, `{"name":"Welcome"}`),
			}}
		case 2:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolAddStep, `{"type":"email","name":"Greeting"}`),
			}}
		case 3:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t3", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"subject":"Hi","body":"Welcome"}}`, cur1.Steps[0].StepID)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Built a welcome email."}
		}
	}}
	b1 := NewAgentLoopBuilder(nil, client1, lister, s.BuilderSessions())
	lc1, _ := driveLoop(t, b1, runInputJSON(t, scope, sid), &cur1, 10)
	if !lc1.completed {
		t.Fatal("turn 1 did not Complete")
	}

	sessAfter1, err := s.BuilderSessions().Get(context.Background(), scope, sid)
	if err != nil {
		t.Fatalf("Get after turn 1: %v", err)
	}
	var draft1 WorkflowDraft
	if err := json.Unmarshal(sessAfter1.DraftWorkflow, &draft1); err != nil {
		t.Fatalf("decode turn-1 draft: %v", err)
	}
	if len(draft1.Steps) != 1 || draft1.Steps[0].Type != "email" {
		t.Fatalf("turn-1 draft = %+v, want one email step", draft1.Steps)
	}
	emailStepID := draft1.Steps[0].StepID

	// --- Between turns: the user sends a follow-up chat message on the session. ---
	if _, err := s.BuilderSessions().AppendMessage(
		context.Background(), scope, sid, "user", "add an sms reminder after the email", "",
	); err != nil {
		t.Fatalf("AppendMessage follow-up: %v", err)
	}

	// --- Turn 2: a fresh run over the SAME session. ---
	var (
		seenSystem string
		seenPrompt string
	)
	client2 := &fnClient{fn: func(call int, req MessageRequest) MessageResponse {
		switch call {
		case 1:
			seenSystem = req.System
			if len(req.Messages) > 0 {
				seenPrompt = req.Messages[0].Text
			}
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("u1", toolAddStep, `{"type":"sms","name":"Nudge"}`),
			}}
		case 2:
			// Find the freshly-added sms step id from the tool_result fed back.
			var smsID string
			for _, m := range req.Messages {
				for _, tr := range m.ToolResults {
					if strings.Contains(tr.Content, `"type":"sms"`) {
						var payload struct {
							Steps []struct {
								StepID string `json:"stepId"`
								Type   string `json:"type"`
							} `json:"steps"`
						}
						_ = json.Unmarshal([]byte(tr.Content), &payload)
						for _, st := range payload.Steps {
							if st.Type == "sms" {
								smsID = st.StepID
							}
						}
					}
				}
			}
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("u2", toolEditStepContent, fmt.Sprintf(`{"stepId":%q,"controlValues":{"body":"Don't forget us"}}`, smsID)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Added an sms reminder."}
		}
	}}
	b2 := NewAgentLoopBuilder(nil, client2, lister, s.BuilderSessions())
	var cur2 WorkflowDraft
	lc2, _ := driveLoop(t, b2, runInputJSON(t, scope, sid), &cur2, 10)
	if !lc2.completed {
		t.Fatal("turn 2 did not Complete")
	}

	// The follow-up turn used the refine (edit) system prompt, seeded from the
	// current draft: it names the workflow and lists the existing email step id.
	if !strings.Contains(seenSystem, "EDITING an existing") {
		t.Errorf("follow-up turn did not use the edit system prompt; got:\n%s", seenSystem)
	}
	if !strings.Contains(seenSystem, emailStepID) {
		t.Errorf("follow-up edit prompt did not list the existing email stepId %q; got:\n%s", emailStepID, seenSystem)
	}
	// The turn was driven by the latest chat message, not the original prompt.
	if seenPrompt != "add an sms reminder after the email" {
		t.Errorf("follow-up instruction = %q, want the appended chat message", seenPrompt)
	}

	// Accumulation: the persisted draft keeps the turn-1 email step and adds the
	// turn-2 sms step.
	sessAfter2, err := s.BuilderSessions().Get(context.Background(), scope, sid)
	if err != nil {
		t.Fatalf("Get after turn 2: %v", err)
	}
	var draft2 WorkflowDraft
	if err := json.Unmarshal(sessAfter2.DraftWorkflow, &draft2); err != nil {
		t.Fatalf("decode turn-2 draft: %v", err)
	}
	if len(draft2.Steps) != 2 {
		t.Fatalf("turn-2 draft has %d steps, want 2 (email accumulated + sms)", len(draft2.Steps))
	}
	if draft2.Steps[0].StepID != emailStepID || draft2.Steps[0].Type != "email" {
		t.Fatalf("turn-1 email step not preserved across the turn: %+v", draft2.Steps[0])
	}
	if draft2.Steps[1].Type != "sms" {
		t.Fatalf("turn-2 second step type = %q, want sms", draft2.Steps[1].Type)
	}
	var cv map[string]string
	_ = json.Unmarshal(draft2.Steps[1].ControlValues, &cv)
	if cv["body"] != "Don't forget us" {
		t.Fatalf("sms body = %q, want the edited value", cv["body"])
	}
}

// TestAgentLoop_CreateFirstTurnStartsEmpty guards the generalized rule at its
// boundary: a create session's first turn (whose stored draft is the empty-object
// placeholder) must start from an EMPTY draft with the CREATE system prompt — not
// be mistaken for a seed and switched to the edit prompt.
func TestAgentLoop_CreateFirstTurnStartsEmpty(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "welcome new users by email")

	var seenSystem string
	client := &fnClient{fn: func(call int, req MessageRequest) MessageResponse {
		if call == 1 {
			seenSystem = req.System
		}
		return MessageResponse{StopReason: "end_turn", Text: "Nothing to do."}
	}}
	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())
	// One iteration is enough: loadState picks the prompt before the model runs.
	lc := &fakeLoopCtx{iter: 0, input: runInputJSON(t, scope, sid)}
	if err := b.handle(context.Background(), lc); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if !strings.Contains(seenSystem, "You design a notification workflow") {
		t.Errorf("create first turn did not use the create system prompt; got:\n%s", seenSystem)
	}
	if strings.Contains(seenSystem, "EDITING an existing") {
		t.Errorf("create first turn wrongly used the edit system prompt; got:\n%s", seenSystem)
	}
}

// TestAgentLoop_ToolCallIDStreamsIntoToolEvent proves the ToolCallID plumbing: a
// mutation tool call's id flows through to the streamed KindTool event so the UI
// (Slice 2) can key a per-call chip to the exact tool call.
func TestAgentLoop_ToolCallIDStreamsIntoToolEvent(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "welcome new users by email")

	var cur WorkflowDraft
	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("call-meta-1", toolSetWorkflowMetadata, `{"name":"Welcome"}`),
			}}
		case 2:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("call-add-42", toolAddStep, `{"type":"email","name":"Greeting"}`),
			}}
		case 3:
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("call-edit-7", toolEditStepContent,
					fmt.Sprintf(`{"stepId":%q,"controlValues":{"subject":"Hi","body":"Welcome"}}`, cur.Steps[0].StepID)),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Done."}
		}
	}}
	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())
	_, streams := driveLoop(t, b, runInputJSON(t, scope, sid), &cur, 10)

	byID := map[string]bool{}
	for _, raw := range streams {
		var ev streamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode stream event: %v", err)
		}
		if ev.Kind == KindTool {
			if ev.ToolCallID == "" {
				t.Errorf("KindTool event carried an empty toolCallId: %+v", ev)
			}
			byID[ev.ToolCallID] = true
		}
	}
	if !byID["call-add-42"] {
		t.Errorf("no KindTool event carried the addStep call id; got ids %v", byID)
	}
	if !byID["call-edit-7"] {
		t.Errorf("no KindTool event carried the editStepContent call id; got ids %v", byID)
	}

	// The id also survives the JSON wire form under the documented key.
	var sawWireKey bool
	for _, raw := range streams {
		if strings.Contains(string(raw), `"toolCallId":"call-add-42"`) {
			sawWireKey = true
		}
	}
	if !sawWireKey {
		t.Errorf("toolCallId not rendered under the wire key; streams: %s", streams)
	}
}
