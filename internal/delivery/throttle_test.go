package delivery

import (
	"context"
	"encoding/json"
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
