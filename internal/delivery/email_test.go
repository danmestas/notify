package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
)

// stubResolver returns a fixed provider and from address (or an error).
type stubResolver struct {
	provider      providers.EmailProvider
	from          string
	providerID    string // defaults to "smtp" when empty
	integrationID string // defaults to "integ-test" when empty
	err           error
}

func (s stubResolver) ResolveEmail(_ context.Context, _ domain.Scope) (providers.EmailProvider, string, string, string, error) {
	pid := s.providerID
	if pid == "" {
		pid = "smtp"
	}
	iid := s.integrationID
	if iid == "" {
		iid = "integ-test"
	}
	return s.provider, s.from, pid, iid, s.err
}

func emailEnvelope() runenv.InAppEnvelope {
	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_abc",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_1",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_1","email":"ada@x.io","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "step_1",
			Channel:         "email",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
		},
	}
}

func TestHandleEmail_RendersSendsPersists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})

	input, err := json.Marshal(emailEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}

	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected exactly one captured email, got %d", len(sent))
	}
	m := sent[0]
	if m.To != "ada@x.io" {
		t.Errorf("To = %q, want %q", m.To, "ada@x.io")
	}
	if m.From != "noreply@acme.io" {
		t.Errorf("From = %q, want %q", m.From, "noreply@acme.io")
	}
	if m.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", m.Subject, "Hi Ada")
	}
	if m.Text != "You have 3 items" {
		t.Errorf("Text = %q, want %q", m.Text, "You have 3 items")
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_1", "email")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one persisted message, got %d", len(msgs))
	}
	pm := msgs[0]
	if pm.Channel != "email" {
		t.Errorf("Channel = %q, want %q", pm.Channel, "email")
	}
	if pm.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", pm.Subject, "Hi Ada")
	}
	if pm.Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", pm.Content, "You have 3 items")
	}
	if pm.Status != "sent" {
		t.Errorf("Status = %q, want %q", pm.Status, "sent")
	}
}

func TestHandleEmail_ResolverError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := NewEmail(s, stubResolver{err: errors.New("no active email integration")})

	input, err := json.Marshal(emailEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	out, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input})
	if err != nil {
		t.Fatalf("HandleEmail returned error, want nil: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty handler output")
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_1", "email")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one persisted message, got %d", len(msgs))
	}
	if msgs[0].Status != "error" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "error")
	}
}

func TestHandleEmail_SkipsWhenChannelDisabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"}).
		WithPreferences(stubChecker{enabled: false})

	input, err := json.Marshal(emailEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}

	if sent := cap.Sent(); len(sent) != 0 {
		t.Fatalf("expected zero captured emails, got %d", len(sent))
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_1", "email")
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

// --- email overrides (Phase-A Wave 2) ---

func TestHandleEmail_StepFromOverridesIntegration(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	// Integration-level from is "noreply@acme.io"; step-level from is "custom@acme.io".
	env := emailEnvelope()
	env.Step.From = "custom@acme.io"
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(sent))
	}
	if sent[0].From != "custom@acme.io" {
		t.Errorf("From = %q, want custom@acme.io (step wins)", sent[0].From)
	}
}

func TestHandleEmail_StepFromEmptyUsesIntegration(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	env := emailEnvelope()
	// env.Step.From is "" (zero value) — integration-level from must be used.
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(sent))
	}
	if sent[0].From != "noreply@acme.io" {
		t.Errorf("From = %q, want noreply@acme.io (integration default)", sent[0].From)
	}
}

func TestHandleEmail_ReplyToForwarded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	env := emailEnvelope()
	env.Step.ReplyTo = "support@acme.io"
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(sent))
	}
	if sent[0].ReplyTo != "support@acme.io" {
		t.Errorf("ReplyTo = %q, want support@acme.io", sent[0].ReplyTo)
	}
}

func TestHandleEmail_ReplyToEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	env := emailEnvelope()
	// ReplyTo is "" (zero value); must not appear on the wire.
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(sent))
	}
	if sent[0].ReplyTo != "" {
		t.Errorf("ReplyTo = %q, want empty", sent[0].ReplyTo)
	}
}

func TestHandleEmail_EditorTypeHTML_SetsHTMLField(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	env := emailEnvelope()
	env.Step.EditorType = "html"
	env.Step.BodyTemplate = "<h1>Hi</h1>"
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(sent))
	}
	if sent[0].HTML != "<h1>Hi</h1>" {
		t.Errorf("HTML = %q, want <h1>Hi</h1>", sent[0].HTML)
	}
	if sent[0].Text != "" {
		t.Errorf("Text = %q, want empty when editorType=html", sent[0].Text)
	}
}

func TestHandleEmail_EditorTypeBlock_SetsTextField(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	env := emailEnvelope()
	env.Step.EditorType = "block"
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"})
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(sent))
	}
	if sent[0].Text == "" {
		t.Errorf("Text is empty, want non-empty when editorType=block")
	}
	if sent[0].HTML != "" {
		t.Errorf("HTML = %q, want empty when editorType=block", sent[0].HTML)
	}
}

func TestHandleEmail_PublishesDeliveryOnSent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	cap := providers.NewCapture()
	pub := &stubPublisher{}
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"}).WithEventBus(pub)

	input, err := json.Marshal(emailEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
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
	if m.Channel != "email" {
		t.Errorf("delivery Channel = %q, want %q", m.Channel, "email")
	}
	if m.Subject != "Hi Ada" {
		t.Errorf("delivery Subject = %q, want %q", m.Subject, "Hi Ada")
	}
}
