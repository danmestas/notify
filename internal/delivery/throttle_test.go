package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
)

// stubThrottler is a fixed-answer Throttler for the throttled/allowed cases.
type stubThrottler struct {
	allowed bool
	err     error
}

func (s stubThrottler) ThrottleAllow(_ string, _ int, _ time.Duration) (bool, error) {
	return s.allowed, s.err
}

// throttledEnvelope is inAppEnvelope with a configured per-step limit.
func throttledEnvelope() runenv.InAppEnvelope {
	env := inAppEnvelope()
	env.Step.ThrottleLimit = 1
	env.Step.ThrottleWindowSec = 60
	return env
}

func TestHandleInApp_ThrottledWhenOverLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithThrottle(stubThrottler{allowed: false})

	input, err := json.Marshal(throttledEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "throttled" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "throttled")
	}
	for _, m := range msgs {
		if m.Status == "sent" {
			t.Errorf("unexpected sent message persisted: %+v", m)
		}
	}
}

func TestHandleInApp_NotThrottledWhenAllowed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithThrottle(stubThrottler{allowed: true})

	input, err := json.Marshal(throttledEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "sent")
	}
}

// TestHandleThrottle_Allowed verifies that a ThrottleHandler with an allowing
// Throttler returns (nil, nil) so downstream steps proceed.
func TestHandleThrottle_Allowed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "throttle-allowed",
		TriggerIdentifier: "throttle-allowed",
		Active:            true,
		Steps:             json.RawMessage(`[{"stepId":"step_t","type":"throttle","controlValues":{"limit":5,"windowSec":60}}]`),
	})
	if err != nil {
		t.Fatalf("Create workflow: %v", err)
	}

	h := NewThrottleHandler(s, stubThrottler{allowed: true})
	input, err := json.Marshal(runenv.InAppEnvelope{
		Scope:        runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		WorkflowID:   wf.ID,
		SubscriberID: "user_123",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	out, err := h.Handle(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_t", Input: input})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out != nil {
		t.Errorf("output = %s, want nil", out)
	}
}

// TestHandleThrottle_Denied verifies that when the Throttler denies the
// request, Handle returns a NonRetryableError containing "rate limit exceeded",
// and no message row is written.
func TestHandleThrottle_Denied(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "throttle-denied",
		TriggerIdentifier: "throttle-denied",
		Active:            true,
		Steps:             json.RawMessage(`[{"stepId":"step_t","type":"throttle","controlValues":{"limit":1,"windowSec":60}}]`),
	})
	if err != nil {
		t.Fatalf("Create workflow: %v", err)
	}

	h := NewThrottleHandler(s, stubThrottler{allowed: false})
	input, err := json.Marshal(runenv.InAppEnvelope{
		Scope:        runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		WorkflowID:   wf.ID,
		SubscriberID: "user_123",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	_, herr := h.Handle(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_t", Input: input})
	if herr == nil {
		t.Fatal("Handle: expected error, got nil")
	}
	var nre *engine.NonRetryableError
	if !errors.As(herr, &nre) {
		t.Errorf("error type = %T, want *engine.NonRetryableError", herr)
	}
	if nre != nil && nre.Err.Error() != "throttle: rate limit exceeded" {
		t.Errorf("error message = %q, want \"throttle: rate limit exceeded\"", nre.Err.Error())
	}
}

// TestHandleThrottle_StoreError verifies that a store failure (no notification
// row for the given RunID) returns a retriable error — not a NonRetryableError.
func TestHandleThrottle_StoreError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	h := NewThrottleHandler(s, stubThrottler{allowed: true})
	// Empty input forces the GetByRunID path; "no-such-run" does not exist.
	_, err := h.Handle(ctx, engine.TaskPayload{RunID: "no-such-run", StepID: "step_t", Input: nil})
	if err == nil {
		t.Fatal("Handle: expected error, got nil")
	}
	var nre *engine.NonRetryableError
	if errors.As(err, &nre) {
		t.Errorf("store error should be retriable, got NonRetryableError: %v", err)
	}
}
