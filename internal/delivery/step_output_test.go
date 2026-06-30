package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// TestConditionMet_StepOutputSource proves that an upstream step's captured
// output is addressable as a named filter source: a condition On='h1'
// Field='statusCode' evaluates against stepOutputs["h1"]. The status code is
// stored as float64 (JSON number) and compared to the string "200"; the filter
// engine coerces both, so EQUAL "200" matches and EQUAL "201" does not.
func TestConditionMet_StepOutputSource(t *testing.T) {
	stepOutputs := map[string]map[string]any{
		"h1": {"statusCode": float64(200), "body": "ok"},
	}
	matchFilters := json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"h1","field":"statusCode","operator":"EQUAL","value":"200"}]}]`)
	if !conditionMet(matchFilters, map[string]any{}, map[string]any{}, stepOutputs) {
		t.Error("conditionMet(statusCode==200) = false, want true")
	}

	missFilters := json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"h1","field":"statusCode","operator":"EQUAL","value":"201"}]}]`)
	if conditionMet(missFilters, map[string]any{}, map[string]any{}, stepOutputs) {
		t.Error("conditionMet(statusCode==201) = true, want false")
	}
}

// TestConditionMet_NilStepOutputs_Unchanged proves the no-action-step path is
// unaffected: a payload condition still evaluates with a nil stepOutputs map.
func TestConditionMet_NilStepOutputs_Unchanged(t *testing.T) {
	filters := json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"payload","field":"plan","operator":"EQUAL","value":"pro"}]}]`)
	if !conditionMet(filters, map[string]any{}, map[string]any{"plan": "pro"}, nil) {
		t.Error("conditionMet(payload.plan==pro, nil stepOutputs) = false, want true")
	}
}

// seedHTTPThenInApp seeds a [http_request h1 -> in_app S1] workflow plus its
// subscriber and a notification for run r1, returning the store-backed handler.
// The in_app body references the upstream http step output via {{steps.h1.*}}.
// filters (when non-nil) is the in_app step's top-level domain.Step.Filters,
// mirroring how authoring persists conditions (a sibling of controlValues).
func seedHTTPThenInApp(t *testing.T, inAppControl, filters json.RawMessage) (*Handler, context.Context, domain.Scope) {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps, err := json.Marshal([]domain.Step{
		{StepID: "h1", Type: "http_request", ControlValues: json.RawMessage(`{"url":"https://x","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`)},
		{StepID: "S1", Type: "in_app", ControlValues: inAppControl, Filters: filters},
	})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Call then notify",
		TriggerIdentifier: "call-then-notify",
		Active:            true,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "user_1", FirstName: "Ada"}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID:           "ntf_1",
		WorkflowID:   wf.ID,
		SubscriberID: "user_1",
		RunID:        "r1",
		Payload:      json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}
	// Capture the upstream http_request response into step_outputs (run r1, h1).
	if err := s.StepOutputs().Save(ctx, scope, "r1", "h1", json.RawMessage(`{"statusCode":200,"body":"ok"}`)); err != nil {
		t.Fatalf("StepOutputs.Save: %v", err)
	}
	return New(s), ctx, scope
}

// TestHandleInApp_NonEnvelopeInput_ExposesStepOutput proves the downstream read:
// the in_app step receives the http_request output (a non-envelope JSON) as its
// Input, so it self-sources, loads the run's step outputs, and renders
// {{steps.h1.statusCode}} as the captured 200.
func TestHandleInApp_NonEnvelopeInput_ExposesStepOutput(t *testing.T) {
	h, ctx, scope := seedHTTPThenInApp(t, json.RawMessage(`{"subject":"R","body":"Status: {{steps.h1.statusCode}}"}`), nil)

	// Input is the upstream http_request output: a non-envelope object. The
	// isRunEnvelope probe must reject it and force the self-source rebuild.
	httpOutput := json.RawMessage(`{"statusCode":200,"body":"ok","headers":{}}`)
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "r1", StepID: "S1", Input: httpOutput}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs, err := h.store.Messages().List(ctx, scope, "user_1", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].Content != "Status: 200" {
		t.Errorf("Content = %q, want %q", msgs[0].Content, "Status: 200")
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want sent", msgs[0].Status)
	}
}

// TestHandleInApp_FilterGatesOnStepOutput proves the filter-gate path: an in_app
// step whose condition requires the upstream http statusCode to equal 403 is
// SKIPPED (fail-closed) when the captured statusCode is 200.
func TestHandleInApp_FilterGatesOnStepOutput(t *testing.T) {
	inApp := json.RawMessage(`{"subject":"R","body":"hi"}`)
	filters := json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"h1","field":"statusCode","operator":"EQUAL","value":"403"}]}]`)
	h, ctx, scope := seedHTTPThenInApp(t, inApp, filters)

	httpOutput := json.RawMessage(`{"statusCode":200,"body":"ok","headers":{}}`)
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "r1", StepID: "S1", Input: httpOutput}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs, err := h.store.Messages().List(ctx, scope, "user_1", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if msgs[0].Status != "skipped" {
		t.Errorf("Status = %q, want skipped (condition statusCode==403 is false for 200)", msgs[0].Status)
	}
}
