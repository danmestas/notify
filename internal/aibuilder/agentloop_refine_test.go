package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// seedRefineSession creates a refine-mode builder session seeded with an existing
// workflow's draft (name + steps) and the slug it targets, returning its id.
func seedRefineSession(t *testing.T, s *store.Store, scope domain.Scope, prompt, slug string, existing WorkflowDraft) string {
	t.Helper()
	dj, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("marshal existing draft: %v", err)
	}
	sess, err := s.BuilderSessions().CreateRefine(context.Background(), scope, prompt, slug, dj)
	if err != nil {
		t.Fatalf("seed refine session: %v", err)
	}
	return sess.ID
}

// TestAgentLoop_RefineSeedsExistingWorkflowAndMutates is the headline refine test:
// iteration 0 starts from the EXISTING workflow's steps (not empty), the refine
// system prompt names the workflow and lists its steps with their stepIds, and a
// scripted "add an SMS step after the delay" instruction mutates the draft by
// referencing an existing stepId — then stops and persists the refined draft.
func TestAgentLoop_RefineSeedsExistingWorkflowAndMutates(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}

	existing := WorkflowDraft{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "step_email1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi","body":"Welcome"}`)},
			{StepID: "step_delay1", Type: "delay", ControlValues: json.RawMessage(`{"type":"regular","amount":1,"unit":"days"}`)},
		},
	}
	sid := seedRefineSession(t, s, scope, "add an SMS step after the delay", "welcome", existing)

	var seenSystem string
	client := &fnClient{fn: func(call int, req MessageRequest) MessageResponse {
		if call == 1 {
			seenSystem = req.System
			// Reference the EXISTING delay step by its stepId — proof the draft
			// was seeded and the id surfaced to the model.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolAddStepInBetween, `{"type":"sms","name":"Nudge","afterStepId":"step_delay1"}`),
			}}
		}
		if call == 2 {
			// The new SMS step is the last one; set its body.
			var sidSMS string
			return func() MessageResponse {
				// Locate the SMS step id from the tool_result the loop fed back.
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
									sidSMS = st.StepID
								}
							}
						}
					}
				}
				return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
					tc("t2", toolEditStepContent, fmt.Sprintf(`{"stepId":%q,"controlValues":{"body":"Don't forget us"}}`, sidSMS)),
				}}
			}()
		}
		return MessageResponse{StopReason: "end_turn", Text: "Added an SMS nudge after the delay."}
	}}

	// email + sms active so both channels are available.
	lister := stubLister{ints: []domain.Integration{
		{Channel: "email", Active: true},
		{Channel: "sms", Active: true},
	}}
	b := NewAgentLoopBuilder(nil, client, lister, s.BuilderSessions())
	var cur WorkflowDraft
	lc, _ := driveLoop(t, b, runInputJSON(t, scope, sid), &cur, 10)

	if !lc.completed {
		t.Fatal("refine loop did not Complete")
	}

	// The refine system prompt named the existing workflow and its steps.
	if !strings.Contains(seenSystem, "Welcome") {
		t.Errorf("refine system prompt did not name the workflow; got:\n%s", seenSystem)
	}
	if !strings.Contains(seenSystem, "step_delay1") || !strings.Contains(seenSystem, "step_email1") {
		t.Errorf("refine system prompt did not list the existing stepIds; got:\n%s", seenSystem)
	}
	if !strings.Contains(strings.ToLower(seenSystem), "editing") && !strings.Contains(strings.ToLower(seenSystem), "existing") {
		t.Errorf("refine system prompt does not read as an edit prompt; got:\n%s", seenSystem)
	}

	// The persisted refined draft keeps the two original steps and inserts the SMS
	// step between the delay-position and the end (afterStepId=step_delay1).
	sess, err := s.BuilderSessions().Get(context.Background(), scope, sid)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	// finish flips the session to the terminal Ready status so the apply endpoint
	// only ever applies a completed draft.
	if sess.Status != store.BuilderSessionReady {
		t.Fatalf("session status = %q, want %q after a successful refine", sess.Status, store.BuilderSessionReady)
	}
	var refined WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &refined); err != nil {
		t.Fatalf("decode refined draft: %v", err)
	}
	if refined.Name != "Welcome" || refined.TriggerIdentifier != "welcome" {
		t.Fatalf("refined metadata = %q/%q, want the original preserved", refined.Name, refined.TriggerIdentifier)
	}
	if len(refined.Steps) != 3 {
		t.Fatalf("refined steps = %d, want 3 (email, delay, sms)", len(refined.Steps))
	}
	// Original steps survive by id.
	if refined.Steps[0].StepID != "step_email1" || refined.Steps[1].StepID != "step_delay1" {
		t.Fatalf("original stepIds not preserved: %+v", refined.Steps)
	}
	if refined.Steps[2].Type != "sms" {
		t.Fatalf("third step type = %q, want sms", refined.Steps[2].Type)
	}
	var cv map[string]string
	_ = json.Unmarshal(refined.Steps[2].ControlValues, &cv)
	if cv["body"] != "Don't forget us" {
		t.Fatalf("sms body = %q, want the edited value", cv["body"])
	}
}
