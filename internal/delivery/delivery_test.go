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
// (the reference platform records a failed message, not a crashed run).
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

func (c stubChecker) ChannelEnabled(_ context.Context, _ domain.Scope, _, _, _ string) (bool, error) {
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

// stubPublisher records each PublishInbox call for assertions.
type stubPublisher struct {
	calls []publishCall
	err   error
}

type publishCall struct {
	envID, subID string
	data         []byte
}

func (p *stubPublisher) PublishInbox(environmentID, subscriberID string, data []byte) error {
	p.calls = append(p.calls, publishCall{envID: environmentID, subID: subscriberID, data: data})
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
