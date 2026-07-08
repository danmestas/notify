package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestAgentLoop_PerToolValidateFeedback_CorrectsWithinCap proves the
// front-loaded steptype.Validate wired into applyToolCall's editStepContent
// case (draft.go) rejects a malformed http_request controlValues blob as an
// IsError tool_result fed back on the VERY NEXT turn — not silently, and not
// deferred to the turn-end Compile — and that a model which corrects itself
// completes well within the self-correct cap (well under maxSelfCorrectAttempts
// and MaxIterations).
func TestAgentLoop_PerToolValidateFeedback_CorrectsWithinCap(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}

	existing := WorkflowDraft{
		Name:              "Webhook",
		TriggerIdentifier: "webhook",
		Steps: []domain.Step{
			{StepID: "step_http1", Type: "http_request", ControlValues: json.RawMessage(`{"url":"","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`)},
		},
	}
	sid := seedRefineSession(t, s, scope, "point it at our webhook", "webhook", existing)

	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			// Malformed: method is not one of GET/POST/PUT/PATCH/DELETE.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t1", toolEditStepContent, `{"stepId":"step_http1","controlValues":{"url":"https://example.com","method":"WOOF","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}}`),
			}}
		case 2:
			// Corrected.
			return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
				tc("t2", toolEditStepContent, `{"stepId":"step_http1","controlValues":{"url":"https://example.com","method":"POST","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}}`),
			}}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Updated the webhook URL."}
		}
	}}

	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())
	var cur WorkflowDraft
	lc, _ := driveLoop(t, b, runInputJSON(t, scope, sid), &cur, 5)

	if !lc.completed {
		t.Fatal("loop did not Complete")
	}
	if client.n > 3 {
		t.Fatalf("model called %d times, want <=3 (self-correct within cap)", client.n)
	}

	// Turn 1's malformed edit surfaced as an IsError tool_result fed into turn 2.
	if len(client.reqs) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(client.reqs))
	}
	req2 := client.reqs[1]
	var sawIsError bool
	for _, m := range req2.Messages {
		for _, tr := range m.ToolResults {
			if tr.ToolCallID == "t1" && tr.IsError {
				sawIsError = true
				if !strings.Contains(tr.Content, "method") {
					t.Errorf("IsError tool_result content = %q, want it to mention method", tr.Content)
				}
			}
		}
	}
	if !sawIsError {
		t.Fatal("malformed editStepContent did not produce an IsError tool_result fed into the next turn")
	}

	sess, err := s.BuilderSessions().Get(context.Background(), scope, sid)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	var persisted WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &persisted); err != nil {
		t.Fatalf("decode persisted: %v", err)
	}
	var cv map[string]string
	_ = json.Unmarshal(persisted.Steps[0].ControlValues, &cv)
	if cv["method"] != "POST" {
		t.Fatalf("persisted method = %q, want POST (the corrected value)", cv["method"])
	}
}

// TestAgentLoop_PerToolValidateFeedback_CapTripsKindError proves that a
// persistently-wrong model (one that never corrects a rejected editStepContent
// call) fails fast with a terminal KindError within maxSelfCorrectAttempts
// iterations, instead of riding the self-correct message all the way to
// DefaultMaxIterations.
func TestAgentLoop_PerToolValidateFeedback_CapTripsKindError(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}

	existing := WorkflowDraft{
		Name:              "Webhook",
		TriggerIdentifier: "webhook",
		Steps: []domain.Step{
			{StepID: "step_http1", Type: "http_request", ControlValues: json.RawMessage(`{"url":"","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`)},
		},
	}
	sid := seedRefineSession(t, s, scope, "point it at our webhook", "webhook", existing)

	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		// Always malformed: the model never corrects.
		return MessageResponse{StopReason: "tool_use", ToolCalls: []ToolCall{
			tc(fmt.Sprintf("t%d", call), toolEditStepContent,
				`{"stepId":"step_http1","controlValues":{"url":"https://example.com","method":"WOOF","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}}`),
		}}
	}}

	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions())

	var (
		checkpoint []byte
		lastErr    error
		lc         *fakeLoopCtx
		iters      int
	)
	for i := 0; i < 10; i++ {
		lc = &fakeLoopCtx{iter: i, checkpoint: checkpoint, input: runInputJSON(t, scope, sid)}
		lastErr = b.handle(context.Background(), lc)
		iters++
		if lastErr != nil {
			break
		}
		if lc.completed {
			t.Fatal("loop unexpectedly Completed with a persistently-wrong model")
		}
		checkpoint = lc.nextCheckpoint
	}

	if lastErr == nil {
		t.Fatal("expected handle to fail once the self-correct cap trips")
	}
	if iters > maxSelfCorrectAttempts {
		t.Fatalf("loop ran %d iterations before failing, want <= %d (self-correct cap)", iters, maxSelfCorrectAttempts)
	}
	if !strings.Contains(lastErr.Error(), "self-correction limit exceeded") {
		t.Fatalf("error = %v, want it to mention the self-correction limit", lastErr)
	}

	var sawError bool
	for _, raw := range lc.streams {
		var ev streamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode stream event: %v", err)
		}
		if ev.Kind == KindError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("no KindError event streamed when the self-correct cap tripped")
	}
}
