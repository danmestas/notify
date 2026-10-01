package delivery

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHandleInApp_PersistsSentMessage(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	env := runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_abc",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "step_1",
			Channel:         "in_app",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
		},
	}
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	out, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input})
	if err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty handler output")
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", m.Subject, "Hi Ada")
	}
	if m.Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", m.Content, "You have 3 items")
	}
	if m.Channel != "in_app" {
		t.Errorf("Channel = %q, want %q", m.Channel, "in_app")
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want %q", m.Status, "sent")
	}
	if m.Seen {
		t.Error("Seen = true, want false")
	}
	if m.Read {
		t.Error("Read = true, want false")
	}
	if m.Archived {
		t.Error("Archived = true, want false")
	}
	if m.TransactionID != "txn_abc" {
		t.Errorf("TransactionID = %q, want %q", m.TransactionID, "txn_abc")
	}
	if m.RunID != "run_1" {
		t.Errorf("RunID = %q, want %q", m.RunID, "run_1")
	}
}

// TestHandleInApp_MalformedEnvelope: a malformed envelope yields no decoded
// scope/subscriber, so the handler records a status:"error" message under the
// empty scope ("","") with an empty subscriber id and returns a nil error
// (a failed message is recorded, not a crashed run).
func TestHandleInApp_MalformedEnvelope(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	out, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_err", Input: json.RawMessage(`{`)})
	if err != nil {
		t.Fatalf("HandleInApp returned error, want nil: %v", err)
	}

	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "error" {
		t.Errorf("Status = %q, want %q", m.Status, "error")
	}
	if m.ErrorText == "" {
		t.Error("ErrorText is empty, want the unmarshal error")
	}

	// The failed message is persisted under the empty (undeterminable) scope.
	msgs, err := s.Messages().List(ctx, domain.Scope{}, "", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one error message, got %d", len(msgs))
	}
	if msgs[0].Status != "error" {
		t.Errorf("persisted Status = %q, want %q", msgs[0].Status, "error")
	}
}

// stubChecker is a fixed-answer PreferenceChecker for the disabled/enabled cases.
type stubChecker struct {
	enabled bool
	err     error
}

func (c stubChecker) ChannelEnabled(_ context.Context, _ domain.Scope, _, _, _ string, _ bool) (bool, error) {
	return c.enabled, c.err
}

func inAppEnvelope() runenv.InAppEnvelope {
	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_abc",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "step_1",
			Channel:         "in_app",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
		},
	}
}

func TestHandleInApp_SkipsWhenChannelDisabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithPreferences(stubChecker{enabled: false})

	input, err := json.Marshal(inAppEnvelope())
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
	if msgs[0].Status != "skipped" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "skipped")
	}
	for _, m := range msgs {
		if m.Status == "sent" {
			t.Errorf("unexpected sent message persisted: %+v", m)
		}
	}
}

func TestHandleInApp_SendsWhenEnabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithPreferences(stubChecker{enabled: true})

	input, err := json.Marshal(inAppEnvelope())
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

// criticalAwareChecker mirrors preferences.Service.ChannelEnabled's real
// contract (critical always wins over a mute) without depending on the
// preferences package. It exists to prove HandleInApp forwards
// env.WorkflowCritical through to the checker rather than deciding the
// override itself — delivery.go centralizes zero critical-bypass logic of its
// own, matching the checker's ChannelEnabled(..., critical bool) contract.
type criticalAwareChecker struct{ muted bool }

func (c criticalAwareChecker) ChannelEnabled(_ context.Context, _ domain.Scope, _, _, _ string, critical bool) (bool, error) {
	if critical {
		return true, nil
	}
	return !c.muted, nil
}

// TestHandleInApp_CriticalWorkflowBypassesMutedChannel is the no-opt-out
// contract at the delivery layer: a critical workflow (e.g. password-reset)
// still delivers ("sent") even though the subscriber has muted the channel.
func TestHandleInApp_CriticalWorkflowBypassesMutedChannel(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithPreferences(criticalAwareChecker{muted: true})

	env := inAppEnvelope()
	env.WorkflowCritical = true
	input, err := json.Marshal(env)
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
		t.Errorf("Status = %q, want %q (critical workflow must bypass the mute)", msgs[0].Status, "sent")
	}
}

// TestHandleInApp_NonCriticalWorkflowStaysMuted is the regression: a
// non-critical workflow (WorkflowCritical false, the default) must still be
// skipped by the same muted channel.
func TestHandleInApp_NonCriticalWorkflowStaysMuted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithPreferences(criticalAwareChecker{muted: true})

	env := inAppEnvelope()
	env.WorkflowCritical = false
	input, err := json.Marshal(env)
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
	if msgs[0].Status != "skipped" {
		t.Errorf("Status = %q, want %q (non-critical workflow must stay muted)", msgs[0].Status, "skipped")
	}
}

// filterEnvelope builds an in_app envelope whose step carries the given raw
// filters JSON, for the per-step condition gating tests.
func filterEnvelope(filters json.RawMessage, payload json.RawMessage) runenv.InAppEnvelope {
	env := inAppEnvelope()
	if payload != nil {
		env.Payload = payload
	}
	env.Step.Filters = filters
	return env
}

// premiumFilter is a single-group, single-condition filter that passes when the
// subscriber's data.plan equals "premium".
var premiumFilter = json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"subscriber","field":"data.plan","operator":"EQUAL","value":"premium"}]}]`)

func msgsFor(t *testing.T, s *store.Store, subscriberID string) []domain.Message {
	t.Helper()
	msgs, err := s.Messages().List(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, subscriberID, "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return msgs
}

// TestHandleInApp_ConditionTrue_Sends: a step whose filter condition evaluates
// true for the subscriber RUNS (one "sent" message).
func TestHandleInApp_ConditionTrue_Sends(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	env := filterEnvelope(premiumFilter, nil)
	env.SubscriberID = "user_premium"
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_premium","firstName":"Ada","data":{"plan":"premium"}}`)
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs := msgsFor(t, s, "user_premium")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "sent")
	}
}

// TestHandleInApp_ConditionFalse_Skips: a step whose filter condition evaluates
// false for the subscriber is SKIPPED (no "sent" message; a "skipped" row).
func TestHandleInApp_ConditionFalse_Skips(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	env := filterEnvelope(premiumFilter, nil)
	env.SubscriberID = "user_free"
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_free","firstName":"Bob","data":{"plan":"free"}}`)
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs := msgsFor(t, s, "user_free")
	for _, m := range msgs {
		if m.Status == "sent" {
			t.Fatalf("condition false but a sent message was persisted: %+v", m)
		}
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message, got %+v", msgs)
	}
}

// TestHandleInApp_NoFilters_AlwaysRuns: a step with absent filters always runs
// (regression: the 99% no-condition path is unchanged).
func TestHandleInApp_NoFilters_AlwaysRuns(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	input, err := json.Marshal(inAppEnvelope()) // no Filters set
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs := msgsFor(t, s, "user_123")
	if len(msgs) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("expected one sent message, got %+v", msgs)
	}
}

// TestHandleInApp_EmptyFiltersArray_AlwaysRuns: an explicit empty filters array
// is equivalent to no condition and must run.
func TestHandleInApp_EmptyFiltersArray_AlwaysRuns(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	env := filterEnvelope(json.RawMessage(`[]`), nil)
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs := msgsFor(t, s, "user_123")
	if len(msgs) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("expected one sent message, got %+v", msgs)
	}
}

// TestHandleInApp_PayloadCondition_Gates: a payload-sourced condition gates the
// send correctly (true -> sent, false -> skipped).
func TestHandleInApp_PayloadCondition_Gates(t *testing.T) {
	payloadFilter := json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"payload","field":"tier","operator":"EQUAL","value":"gold"}]}]`)

	t.Run("true sends", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		h := New(s)
		env := filterEnvelope(payloadFilter, json.RawMessage(`{"tier":"gold","itemCount":3}`))
		env.SubscriberID = "p_gold"
		env.Subscriber = json.RawMessage(`{"subscriberId":"p_gold","firstName":"Ada"}`)
		input, _ := json.Marshal(env)
		if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", Input: input}); err != nil {
			t.Fatalf("HandleInApp: %v", err)
		}
		msgs := msgsFor(t, s, "p_gold")
		if len(msgs) != 1 || msgs[0].Status != "sent" {
			t.Fatalf("expected one sent message, got %+v", msgs)
		}
	})

	t.Run("false skips", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		h := New(s)
		env := filterEnvelope(payloadFilter, json.RawMessage(`{"tier":"silver","itemCount":3}`))
		env.SubscriberID = "p_silver"
		env.Subscriber = json.RawMessage(`{"subscriberId":"p_silver","firstName":"Bob"}`)
		input, _ := json.Marshal(env)
		if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", Input: input}); err != nil {
			t.Fatalf("HandleInApp: %v", err)
		}
		msgs := msgsFor(t, s, "p_silver")
		for _, m := range msgs {
			if m.Status == "sent" {
				t.Fatalf("condition false but a sent message was persisted: %+v", m)
			}
		}
		if len(msgs) != 1 || msgs[0].Status != "skipped" {
			t.Fatalf("expected one skipped message, got %+v", msgs)
		}
	})
}

// TestHandleInApp_MalformedFilters_FailsClosed: a malformed filters JSON must
// fail closed (skip, no send) without panicking.
func TestHandleInApp_MalformedFilters_FailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	// Filters that are valid JSON (so the envelope itself decodes) but the wrong
	// shape for a filter array (an object, not an array of Group) — this models
	// a corrupted/garbage persisted condition. Unmarshaling it into []filter.Group
	// errors, which must fail closed (skip), not fall through to a send.
	env := filterEnvelope(json.RawMessage(`{"corrupt":"not an array of groups"}`), nil)
	env.SubscriberID = "user_bad"
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_bad","firstName":"Eve"}`)
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp returned error, want nil: %v", err)
	}

	msgs := msgsFor(t, s, "user_bad")
	for _, m := range msgs {
		if m.Status == "sent" {
			t.Fatalf("malformed filters but a sent message was persisted (fail-open): %+v", m)
		}
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message (fail-closed), got %+v", msgs)
	}
}

// stubPublisher records each PublishInbox and PublishDelivery call for assertions.
type stubPublisher struct {
	calls         []publishCall
	deliveryCalls []deliveryCall
	err           error
}

type publishCall struct {
	envID, subID string
	data         []byte
}

type deliveryCall struct {
	envID string
	data  []byte
}

func (p *stubPublisher) PublishInbox(environmentID, subscriberID string, data []byte) error {
	p.calls = append(p.calls, publishCall{envID: environmentID, subID: subscriberID, data: data})
	return p.err
}

func (p *stubPublisher) PublishDelivery(environmentID string, data []byte) error {
	p.deliveryCalls = append(p.deliveryCalls, deliveryCall{envID: environmentID, data: data})
	return p.err
}

func TestHandleInApp_PublishesInboxOnSent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pub := &stubPublisher{}
	h := New(s).WithEventBus(pub)

	input, err := json.Marshal(inAppEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	if len(pub.calls) != 1 {
		t.Fatalf("expected exactly one PublishInbox call, got %d", len(pub.calls))
	}
	c := pub.calls[0]
	if c.envID != "env_1" {
		t.Errorf("environmentID = %q, want %q", c.envID, "env_1")
	}
	if c.subID != "user_123" {
		t.Errorf("subscriberID = %q, want %q", c.subID, "user_123")
	}
	var m domain.Message
	if err := json.Unmarshal(c.data, &m); err != nil {
		t.Fatalf("unmarshal published data: %v", err)
	}
	if m.Status != "sent" {
		t.Errorf("published Status = %q, want %q", m.Status, "sent")
	}
	if m.Subject != "Hi Ada" {
		t.Errorf("published Subject = %q, want %q", m.Subject, "Hi Ada")
	}
	if m.Content != "You have 3 items" {
		t.Errorf("published Content = %q, want %q", m.Content, "You have 3 items")
	}
}

func TestHandleInApp_NoPublishWhenSkipped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pub := &stubPublisher{}
	h := New(s).WithPreferences(stubChecker{enabled: false}).WithEventBus(pub)

	input, err := json.Marshal(inAppEnvelope())
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
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message, got %+v", msgs)
	}
	if len(pub.calls) != 0 {
		t.Errorf("expected zero PublishInbox calls on skip, got %d", len(pub.calls))
	}
}

func TestHandleInApp_NilPublisher_NoPanic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	input, err := json.Marshal(inAppEnvelope())
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
	if len(msgs) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("expected one sent message, got %+v", msgs)
	}
}

func TestHandleInApp_PublishesDeliveryOnSent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pub := &stubPublisher{}
	h := New(s).WithEventBus(pub)

	input, err := json.Marshal(inAppEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	if len(pub.deliveryCalls) != 1 {
		t.Fatalf("expected exactly one PublishDelivery call, got %d", len(pub.deliveryCalls))
	}
	c := pub.deliveryCalls[0]
	if c.envID != "env_1" {
		t.Errorf("delivery environmentID = %q, want %q", c.envID, "env_1")
	}
	var m domain.Message
	if err := json.Unmarshal(c.data, &m); err != nil {
		t.Fatalf("unmarshal delivery data: %v", err)
	}
	if m.Status != "sent" {
		t.Errorf("delivery Status = %q, want %q", m.Status, "sent")
	}
	if m.Channel != "in_app" {
		t.Errorf("delivery Channel = %q, want %q", m.Channel, "in_app")
	}
}

func TestHandleInApp_PublishesDeliveryOnSkipped(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pub := &stubPublisher{}
	h := New(s).WithPreferences(stubChecker{enabled: false}).WithEventBus(pub)

	input, err := json.Marshal(inAppEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	// No inbox publish on skip, but a delivery-status event still fires.
	if len(pub.calls) != 0 {
		t.Errorf("expected zero PublishInbox calls on skip, got %d", len(pub.calls))
	}
	if len(pub.deliveryCalls) != 1 {
		t.Fatalf("expected exactly one PublishDelivery call on skip, got %d", len(pub.deliveryCalls))
	}
	var m domain.Message
	if err := json.Unmarshal(pub.deliveryCalls[0].data, &m); err != nil {
		t.Fatalf("unmarshal delivery data: %v", err)
	}
	if m.Status != "skipped" {
		t.Errorf("delivery Status = %q, want %q", m.Status, "skipped")
	}
}

// TestHandleInApp_SelfSourcesWhenInputEmpty proves the self-source fallback:
// when TaskPayload.Input is empty (e.g. the step DependsOn a delay whose output
// is nil), the handler rebuilds the envelope from the store by run id and still
// renders + persists a correct in_app message.
func TestHandleInApp_SelfSourcesWhenInputEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps := json.RawMessage(`[{"stepId":"S1","type":"in_app","controlValues":{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}}]`)
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Welcome",
		TriggerIdentifier: "welcome-flow",
		Active:            true,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}

	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{
		SubscriberID: "user_123",
		FirstName:    "Ada",
	}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}

	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID:            "ntf_1",
		WorkflowID:    wf.ID,
		SubscriberID:  "user_123",
		TransactionID: "txn_abc",
		RunID:         "r9",
		Payload:       json.RawMessage(`{"itemCount":3}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}

	// Input is nil: forces the self-source path.
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "r9", StepID: "S1", Input: nil}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs, err := s.Messages().List(ctx, scope, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", m.Subject, "Hi Ada")
	}
	if m.Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", m.Content, "You have 3 items")
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want %q", m.Status, "sent")
	}
	if m.TransactionID != "txn_abc" {
		t.Errorf("TransactionID = %q, want %q", m.TransactionID, "txn_abc")
	}
	if m.NotificationID != "ntf_1" {
		t.Errorf("NotificationID = %q, want %q", m.NotificationID, "ntf_1")
	}
}

// stubDigester returns a fixed event slice for any key, standing in for
// eventbus.Bus.DigestDrain in the unit test.
type stubDigester struct {
	events []json.RawMessage
}

func (d stubDigester) DigestDrain(string) ([]json.RawMessage, error) {
	return d.events, nil
}

// TestHandleInApp_DigestDrainsAndCounts proves the self-source digest path:
// a notification carrying a DigestKey drains its merged events and exposes
// {{digest.count}} so the body renders the aggregate count, persisting ONE
// "sent" message.
func TestHandleInApp_DigestDrainsAndCounts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithDigester(stubDigester{events: []json.RawMessage{
		json.RawMessage(`{}`), json.RawMessage(`{}`), json.RawMessage(`{}`),
	}})

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps := json.RawMessage(`[{"stepId":"dg","type":"digest","controlValues":{"amount":1,"unit":"seconds"}},{"stepId":"S1","type":"in_app","controlValues":{"subject":"Digest","body":"You have {{digest.count}} new items"}}]`)
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Digested",
		TriggerIdentifier: "digested",
		Active:            true,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}

	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{
		SubscriberID: "dg_user",
	}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}

	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID:            "ntf_dg",
		WorkflowID:    wf.ID,
		SubscriberID:  "dg_user",
		TransactionID: "txn_dg",
		RunID:         "r_dg",
		DigestKey:     "k",
		Payload:       json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}

	// Input nil -> self-source -> drain DigestKey "k" -> count 3.
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "r_dg", StepID: "S1", Input: nil}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	msgs, err := s.Messages().List(ctx, scope, "dg_user", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Content != "You have 3 new items" {
		t.Errorf("Content = %q, want %q", msgs[0].Content, "You have 3 new items")
	}
	if msgs[0].Status != "sent" {
		t.Errorf("Status = %q, want sent", msgs[0].Status)
	}
}
