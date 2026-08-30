package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		WorkflowID:     wf.ID,
		NotificationID: "ntf_allowed",
		SubscriberID:   "user_123",
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

	msgs, err := s.Messages().ListByNotificationID(ctx, scope, "ntf_allowed")
	if err != nil {
		t.Fatalf("ListByNotificationID: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages written on allow, got %d", len(msgs))
	}
}

// TestHandleThrottle_Denied verifies that when the Throttler denies the
// request, Handle persists exactly one "throttled" message row (correctly
// attributed) and still returns a NonRetryableError containing "rate limit
// exceeded" -- suppression is observable, but the run still stops so no
// downstream channel step executes (check.throttle compiles to a plain
// sequential dagnats task, so only a permanent task failure halts the DAG).
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
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_1",
		WorkflowID:     wf.ID,
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
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

	msgs, err := s.Messages().ListByNotificationID(ctx, scope, "ntf_1")
	if err != nil {
		t.Fatalf("ListByNotificationID: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	got := msgs[0]
	if got.Status != "throttled" {
		t.Errorf("Status = %q, want %q", got.Status, "throttled")
	}
	if got.OrgID != "org_1" || got.EnvID != "env_1" {
		t.Errorf("scope = %q/%q, want org_1/env_1", got.OrgID, got.EnvID)
	}
	if got.WorkflowID != wf.ID {
		t.Errorf("WorkflowID = %q, want %q", got.WorkflowID, wf.ID)
	}
	if got.SubscriberID != "user_123" {
		t.Errorf("SubscriberID = %q, want %q", got.SubscriberID, "user_123")
	}
	if got.RunID != "run_1" || got.StepID != "step_t" {
		t.Errorf("RunID/StepID = %q/%q, want run_1/step_t", got.RunID, got.StepID)
	}
	if got.TransactionID != "txn_1" {
		t.Errorf("TransactionID = %q, want %q", got.TransactionID, "txn_1")
	}
	if got.ProviderID != "" || got.IntegrationID != "" || got.Channel != "" {
		t.Errorf("expected no provider/integration/channel (no channel ran), got provider=%q integration=%q channel=%q", got.ProviderID, got.IntegrationID, got.Channel)
	}
}

// TestHandleThrottle_DeniedPersistsEvenWithoutInput covers the else branch of
// Handle (throttle follows a sleep/delay step, so t.Input is empty and
// identifiers are rebuilt from the notification row via GetByRunID) --
// denial must still persist a throttled message row attributed from that
// rebuilt notification.
func TestHandleThrottle_DeniedPersistsEvenWithoutInput(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "throttle-denied-2",
		TriggerIdentifier: "throttle-denied-2",
		Active:            true,
		Steps:             json.RawMessage(`[{"stepId":"step_t","type":"throttle","controlValues":{"limit":1,"windowSec":60}}]`),
	})
	if err != nil {
		t.Fatalf("Create workflow: %v", err)
	}
	notif, err := s.Notifications().Create(ctx, scope, domain.Notification{
		WorkflowID:    wf.ID,
		SubscriberID:  "user_456",
		TransactionID: "txn_2",
		RunID:         "run_2",
		Payload:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Create notification: %v", err)
	}

	h := NewThrottleHandler(s, stubThrottler{allowed: false})
	_, herr := h.Handle(ctx, engine.TaskPayload{RunID: "run_2", StepID: "step_t", Input: nil})
	if herr == nil {
		t.Fatal("Handle: expected error, got nil")
	}

	msgs, err := s.Messages().ListByNotificationID(ctx, scope, notif.ID)
	if err != nil {
		t.Fatalf("ListByNotificationID: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "throttled" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "throttled")
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

// keyCapturingThrottler records every key it is asked about so a test can
// assert how the budget was partitioned.
type keyCapturingThrottler struct {
	mu   sync.Mutex
	keys []string
}

func (k *keyCapturingThrottler) ThrottleAllow(key string, _ int, _ time.Duration) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
	return true, nil
}

func (k *keyCapturingThrottler) seen() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

// throttleKeyFixture seeds a workflow whose throttle step carries controlValues
// and returns the store, handler, key recorder and workflow id.
func throttleKeyFixture(t *testing.T, controlValues string) (*keyCapturingThrottler, *ThrottleHandler, string) {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "throttle-key",
		TriggerIdentifier: "throttle-key",
		Active:            true,
		Steps:             json.RawMessage(`[{"stepId":"step_t","type":"throttle","controlValues":` + controlValues + `}]`),
	})
	if err != nil {
		t.Fatalf("Create workflow: %v", err)
	}
	rec := &keyCapturingThrottler{}
	return rec, NewThrottleHandler(s, rec), wf.ID
}

// fireThrottle drives Handle once with the given payload.
func fireThrottle(t *testing.T, h *ThrottleHandler, workflowID, payload string) {
	t.Helper()
	input, err := json.Marshal(runenv.InAppEnvelope{
		Scope:        runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		WorkflowID:   workflowID,
		SubscriberID: "user_123",
		Payload:      json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.Handle(context.Background(), engine.TaskPayload{RunID: "run_1", StepID: "step_t", Input: input}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// TestHandleThrottle_ThrottleKeyPartitionsBudget is the #295 repro: two
// unrelated alerts on one workflow must not share an allowance. With a
// throttleKey configured they resolve to different keys, so a flapping check
// can no longer consume the budget a different page needs.
func TestHandleThrottle_ThrottleKeyPartitionsBudget(t *testing.T) {
	rec, h, wfID := throttleKeyFixture(t, `{"limit":3,"windowSec":3600,"throttleKey":"payload.check"}`)

	fireThrottle(t, h, wfID, `{"check":"disk-space"}`)
	fireThrottle(t, h, wfID, `{"check":"dagnats-down"}`)

	keys := rec.seen()
	if len(keys) != 2 {
		t.Fatalf("throttler saw %d keys, want 2", len(keys))
	}
	if keys[0] == keys[1] {
		t.Fatalf("both alerts shared key %q: a flapping check would still suppress an unrelated page", keys[0])
	}
}

// TestHandleThrottle_NoThrottleKeyKeepsOneBudget pins the unchanged default:
// without a throttleKey every payload shares the (env, subscriber, workflow)
// budget exactly as before.
func TestHandleThrottle_NoThrottleKeyKeepsOneBudget(t *testing.T) {
	rec, h, wfID := throttleKeyFixture(t, `{"limit":3,"windowSec":3600}`)

	fireThrottle(t, h, wfID, `{"check":"disk-space"}`)
	fireThrottle(t, h, wfID, `{"check":"dagnats-down"}`)

	keys := rec.seen()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("keys = %q, want one shared key for both payloads", keys)
	}
}

// TestHandleThrottle_ThrottleKeyMissingFieldFallsBack: a configured key whose
// field is absent from the payload must fall back to the base key rather than
// inventing a partition per event (which would disable throttling entirely).
func TestHandleThrottle_ThrottleKeyMissingFieldFallsBack(t *testing.T) {
	rec, h, wfID := throttleKeyFixture(t, `{"limit":3,"windowSec":3600,"throttleKey":"payload.check"}`)

	fireThrottle(t, h, wfID, `{"other":"a"}`)
	fireThrottle(t, h, wfID, `{"other":"b"}`)

	keys := rec.seen()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("keys = %q, want the base key for both (missing field must not partition)", keys)
	}
}
