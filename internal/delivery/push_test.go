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
	"github.com/danmestas/notify/internal/store"
)

// stubPushResolver is a fixed-answer PushResolver standing in for
// connectivity.Service in the unit tests.
type stubPushResolver struct {
	p             providers.PushProvider
	id            string // providerID
	integrationID string // defaults to "integ-push-test" when empty
	err           error
}

func (s stubPushResolver) ResolvePush(_ context.Context, _ domain.Scope) (providers.PushProvider, string, string, error) {
	iid := s.integrationID
	if iid == "" {
		iid = "integ-push-test"
	}
	return s.p, s.id, iid, s.err
}

// failingPushProvider always fails with the configured error.
type failingPushProvider struct{ err error }

func (p failingPushProvider) Send(_ context.Context, _ providers.PushMessage) (providers.Result, error) {
	return providers.Result{}, p.err
}

// pushEnvelope mirrors inAppEnvelope for the push channel, including the push
// extras (priority/tags/markdown) the wire carries.
func pushEnvelope() runenv.InAppEnvelope {
	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_push",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"itemCount":3}`),
		Step: runenv.Step{
			StepID:          "P1",
			Channel:         "push",
			SubjectTemplate: "Hi {{subscriber.firstName}}",
			BodyTemplate:    "You have {{payload.itemCount}} items",
			Priority:        4,
			Tags:            []string{"warning", "skull"},
			Markdown:        true,
		},
	}
}

// seedTopic registers the subscriber's ntfy topic in subscriber_channels.
func seedTopic(t *testing.T, s *store.Store, topic string) {
	t.Helper()
	if _, err := s.SubscriberChannels().Upsert(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, domain.SubscriberChannel{
		SubscriberID: "user_123",
		ProviderID:   "ntfy",
		Type:         "push",
		Credentials:  json.RawMessage(`{"topic":"` + topic + `"}`),
	}); err != nil {
		t.Fatalf("SubscriberChannels.Upsert: %v", err)
	}
}

func TestHandlePush_RendersSendsPersists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTopic(t, s, "agent-alerts")
	capt := providers.NewCapturePush()
	h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_p1", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	// --- persisted message oracle ---
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
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
	if m.Channel != "push" {
		t.Errorf("Channel = %q, want %q", m.Channel, "push")
	}
	if m.ProviderID != "ntfy" {
		t.Errorf("ProviderID = %q, want %q", m.ProviderID, "ntfy")
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want %q", m.Status, "sent")
	}

	// --- capture oracle: topic from subscriber_channels + push extras ---
	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("capt.Sent() len = %d, want 1", len(sent))
	}
	pm := sent[0]
	if pm.Topic != "agent-alerts" {
		t.Errorf("Topic = %q, want %q", pm.Topic, "agent-alerts")
	}
	if pm.Title != "Hi Ada" {
		t.Errorf("Title = %q, want %q", pm.Title, "Hi Ada")
	}
	if pm.Body != "You have 3 items" {
		t.Errorf("Body = %q, want %q", pm.Body, "You have 3 items")
	}
	if pm.Priority != 4 {
		t.Errorf("Priority = %d, want 4", pm.Priority)
	}
	if len(pm.Tags) != 2 || pm.Tags[0] != "warning" || pm.Tags[1] != "skull" {
		t.Errorf("Tags = %v, want [warning skull]", pm.Tags)
	}
	if !pm.Markdown {
		t.Error("Markdown = false, want true")
	}
}

func TestHandlePush_SkipsWhenChannelDisabled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTopic(t, s, "agent-alerts")
	capt := providers.NewCapturePush()
	h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"}).WithPreferences(stubChecker{enabled: false})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_p2", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	if msgs[0].Status != "skipped" {
		t.Errorf("Status = %q, want %q", msgs[0].Status, "skipped")
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times on skip, want 0", got)
	}
}

func TestHandlePush_ThrottledWhenOverLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTopic(t, s, "agent-alerts")
	capt := providers.NewCapturePush()
	h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"}).WithThrottle(stubThrottler{allowed: false})

	env := pushEnvelope()
	env.Step.ThrottleLimit = 1
	env.Step.ThrottleWindowSec = 60
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_p3", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "throttled" {
		t.Fatalf("expected one throttled message, got %+v", msgs)
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times when throttled, want 0", got)
	}
}

// TestHandlePush_MissingTopicRecordsError: a subscriber with no
// subscriber_channels row for the provider is a permanent fault — a recorded
// "error" message, not a crashed (or retried) run.
func TestHandlePush_MissingTopicRecordsError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	capt := providers.NewCapturePush()
	h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_p4", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush returned error, want nil: %v", err)
	}
	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "error" {
		t.Errorf("Status = %q, want %q", m.Status, "error")
	}
	if m.ErrorText == "" {
		t.Error("ErrorText is empty, want the missing-channel cause")
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times without a topic, want 0", got)
	}
}

// TestHandlePush_PermanentSendErrorRecordsError: a permanent provider failure
// (4xx — a plain error) records a status:"error" message with a nil error.
func TestHandlePush_PermanentSendErrorRecordsError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTopic(t, s, "agent-alerts")
	h := NewPush(s, stubPushResolver{p: failingPushProvider{err: errors.New("ntfy: status 403: forbidden")}, id: "ntfy"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_p5", StepID: "P1", Input: input})
	if err != nil {
		t.Fatalf("HandlePush returned error, want nil: %v", err)
	}
	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "error" {
		t.Errorf("Status = %q, want %q", m.Status, "error")
	}
	if m.ErrorText != "ntfy: status 403: forbidden" {
		t.Errorf("ErrorText = %q, want the provider cause", m.ErrorText)
	}
}

// TestHandlePush_TransientSendErrorIsRetryable: a transient provider failure
// (5xx/timeout) surfaces as engine.RetryableError so the engine retries the
// task, and no terminal message row is recorded.
func TestHandlePush_TransientSendErrorIsRetryable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTopic(t, s, "agent-alerts")
	cause := providers.NewTransientError(errors.New("ntfy: status 503"))
	h := NewPush(s, stubPushResolver{p: failingPushProvider{err: cause}, id: "ntfy"})

	input, err := json.Marshal(pushEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	_, herr := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_p6", StepID: "P1", Input: input})
	if herr == nil {
		t.Fatal("HandlePush on transient fault: expected error, got nil")
	}
	var re *engine.RetryableError
	if !errors.As(herr, &re) {
		t.Errorf("error %v is not engine.RetryableError", herr)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "push")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no terminal message before retry, got %d", len(msgs))
	}
}
