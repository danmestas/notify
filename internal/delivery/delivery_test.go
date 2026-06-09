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
