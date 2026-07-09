package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
)

// errPublishBoom is a fixed sentinel error for the publish-failure test.
var errPublishBoom = errors.New("boom")

// stubFeedPublisher records each PublishFeed call for assertions, standing in
// for eventbus.Bus.PublishFeed in the unit test.
type stubFeedPublisher struct {
	calls []feedCall
	err   error
}

type feedCall struct {
	envID, topic string
	data         []byte
}

func (p *stubFeedPublisher) PublishFeed(environmentID, topic string, data []byte) error {
	p.calls = append(p.calls, feedCall{envID: environmentID, topic: topic, data: data})
	return p.err
}

// streamEnvelope builds a baseline stream-channel envelope: a dot-key Topic
// template and a plain BodyTemplate, both rendered against {subscriber,payload}.
func streamEnvelope() runenv.InAppEnvelope {
	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: "org_1", EnvID: "env_1"},
		TransactionID:  "txn_abc",
		WorkflowID:     "wf_1",
		NotificationID: "ntf_1",
		SubscriberID:   "user_123",
		Subscriber:     json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`),
		Payload:        json.RawMessage(`{"region":"eu","n":3}`),
		Step: runenv.Step{
			StepID:       "step_1",
			Channel:      "stream",
			Topic:        "orders.{{payload.region}}",
			BodyTemplate: "{{payload.n}} created",
		},
	}
}

// TestHandleStream_RendersAndPublishes proves the full render -> gates ->
// persist -> publish path: the rendered topic and body are correct, the
// saved message carries the rendered topic in Subject (Content carries the
// rendered body), and both the feed and the delivery-status event fire once.
func TestHandleStream_RendersAndPublishes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := &stubFeedPublisher{}
	pub := &stubPublisher{}
	h := NewStream(s).WithEventBus(pub).WithFeed(feed)

	input, err := json.Marshal(streamEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	out, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input})
	if err != nil {
		t.Fatalf("HandleStream: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty handler output")
	}

	if len(feed.calls) != 1 {
		t.Fatalf("expected exactly one PublishFeed call, got %d", len(feed.calls))
	}
	fc := feed.calls[0]
	if fc.envID != "env_1" {
		t.Errorf("PublishFeed environmentID = %q, want %q", fc.envID, "env_1")
	}
	if fc.topic != "orders.eu" {
		t.Errorf("PublishFeed topic = %q, want %q", fc.topic, "orders.eu")
	}
	var published domain.Message
	if err := json.Unmarshal(fc.data, &published); err != nil {
		t.Fatalf("unmarshal published feed data: %v", err)
	}
	if published.Status != "sent" {
		t.Errorf("published Status = %q, want %q", published.Status, "sent")
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "stream")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Channel != "stream" {
		t.Errorf("Channel = %q, want %q", m.Channel, "stream")
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want %q", m.Status, "sent")
	}
	if m.Subject != "orders.eu" {
		t.Errorf("Subject = %q, want %q (rendered topic)", m.Subject, "orders.eu")
	}
	if m.Content != "3 created" {
		t.Errorf("Content = %q, want %q", m.Content, "3 created")
	}

	if len(pub.deliveryCalls) != 1 {
		t.Fatalf("expected exactly one PublishDelivery call, got %d", len(pub.deliveryCalls))
	}
	if pub.deliveryCalls[0].envID != "env_1" {
		t.Errorf("PublishDelivery environmentID = %q, want %q", pub.deliveryCalls[0].envID, "env_1")
	}
	// A stream message is not an inbox message: no PublishInbox call.
	if len(pub.calls) != 0 {
		t.Errorf("expected zero PublishInbox calls, got %d", len(pub.calls))
	}
}

// TestHandleStream_InvalidRenderedTopic_FailsClosed proves injection defense:
// when the rendered (not authored) topic fails domain.ValidateFeedTopic, the
// handler persists a status:"error" message with an ErrorText mentioning
// "topic", never calls PublishFeed, still fires the delivery-status event,
// and returns a nil error (failed message, not a crashed run).
func TestHandleStream_InvalidRenderedTopic_FailsClosed(t *testing.T) {
	cases := []struct {
		name          string
		topicTemplate string
		payload       string
	}{
		{"wildcard-star", "orders.{{payload.suffix}}", `{"suffix":"*"}`},
		{"internal-space", "{{payload.topic}}", `{"topic":"a b"}`},
		{"gt-wildcard", "x.{{payload.suffix}}", `{"suffix":">"}`},
		{"empty", "{{payload.topic}}", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			feed := &stubFeedPublisher{}
			pub := &stubPublisher{}
			h := NewStream(s).WithEventBus(pub).WithFeed(feed)

			env := streamEnvelope()
			env.Step.Topic = tc.topicTemplate
			env.Payload = json.RawMessage(tc.payload)
			input, err := json.Marshal(env)
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}

			out, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "run_x", StepID: "step_1", Input: input})
			if err != nil {
				t.Fatalf("HandleStream returned error, want nil: %v", err)
			}

			var m domain.Message
			if uerr := json.Unmarshal(out, &m); uerr != nil {
				t.Fatalf("unmarshal handler output: %v", uerr)
			}
			if m.Status != "error" {
				t.Errorf("Status = %q, want %q", m.Status, "error")
			}
			if !strings.Contains(m.ErrorText, "topic") {
				t.Errorf("ErrorText = %q, want it to mention %q", m.ErrorText, "topic")
			}

			if len(feed.calls) != 0 {
				t.Errorf("expected zero PublishFeed calls, got %d", len(feed.calls))
			}
			if len(pub.deliveryCalls) != 1 {
				t.Fatalf("expected exactly one PublishDelivery call (failure still observable), got %d", len(pub.deliveryCalls))
			}
		})
	}
}

// TestHandleStream_FilterSkips: a step whose filter condition evaluates false
// SKIPS the send — no PublishFeed, still a PublishDelivery.
func TestHandleStream_FilterSkips(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := &stubFeedPublisher{}
	pub := &stubPublisher{}
	h := NewStream(s).WithEventBus(pub).WithFeed(feed)

	env := streamEnvelope()
	env.Step.Filters = premiumFilter
	env.SubscriberID = "user_free"
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_free","firstName":"Bob","data":{"plan":"free"}}`)
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	if _, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleStream: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_free", "stream")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message, got %+v", msgs)
	}
	if len(feed.calls) != 0 {
		t.Errorf("expected zero PublishFeed calls on skip, got %d", len(feed.calls))
	}
	if len(pub.deliveryCalls) != 1 {
		t.Errorf("expected exactly one PublishDelivery call on skip, got %d", len(pub.deliveryCalls))
	}
}

// TestHandleStream_ThrottleRecordsThrottled: a send over the configured
// per-(env,subscriber,workflow) limit records a "throttled" message, never
// publishes to the feed, but still fires the delivery-status event.
func TestHandleStream_ThrottleRecordsThrottled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := &stubFeedPublisher{}
	pub := &stubPublisher{}
	h := NewStream(s).WithEventBus(pub).WithFeed(feed).WithThrottle(stubThrottler{allowed: false})

	env := streamEnvelope()
	env.Step.ThrottleLimit = 1
	env.Step.ThrottleWindowSec = 60
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	if _, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleStream: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "stream")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "throttled" {
		t.Fatalf("expected one throttled message, got %+v", msgs)
	}
	if len(feed.calls) != 0 {
		t.Errorf("expected zero PublishFeed calls when throttled, got %d", len(feed.calls))
	}
	if len(pub.deliveryCalls) != 1 {
		t.Errorf("expected exactly one PublishDelivery call when throttled, got %d", len(pub.deliveryCalls))
	}
}

// TestHandleStream_PreferenceSkips: a disabled "stream" channel preference
// SKIPS the send — no PublishFeed, still a PublishDelivery.
func TestHandleStream_PreferenceSkips(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := &stubFeedPublisher{}
	pub := &stubPublisher{}
	h := NewStream(s).WithPreferences(stubChecker{enabled: false}).WithEventBus(pub).WithFeed(feed)

	input, err := json.Marshal(streamEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	if _, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleStream: %v", err)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "stream")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" {
		t.Fatalf("expected one skipped message, got %+v", msgs)
	}
	if len(feed.calls) != 0 {
		t.Errorf("expected zero PublishFeed calls on skip, got %d", len(feed.calls))
	}
	if len(pub.deliveryCalls) != 1 {
		t.Errorf("expected exactly one PublishDelivery call on skip, got %d", len(pub.deliveryCalls))
	}
}

// TestHandleStream_SelfSourceRebuild proves the self-source (rebuild) path:
// when TaskPayload.Input is empty, the handler rebuilds the envelope from the
// store by run id (A3's Topic plumbing end-to-end at the handler level) and
// still renders + persists + publishes a correct stream message.
func TestHandleStream_SelfSourceRebuild(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := &stubFeedPublisher{}
	h := NewStream(s).WithFeed(feed)

	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	steps, err := json.Marshal([]domain.Step{
		{StepID: "S1", Type: "stream", ControlValues: json.RawMessage(`{"topic":"orders.{{payload.region}}","body":"{{payload.n}} created"}`)},
	})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Stream self-source",
		TriggerIdentifier: "stream-self-source",
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
		Payload:      json.RawMessage(`{"region":"eu","n":3}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}

	// Input is nil: forces the self-source path.
	if _, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "r1", StepID: "S1", Input: nil}); err != nil {
		t.Fatalf("HandleStream: %v", err)
	}

	msgs, err := s.Messages().List(ctx, scope, "user_1", "stream")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Status != "sent" {
		t.Errorf("Status = %q, want %q", m.Status, "sent")
	}
	if m.Subject != "orders.eu" {
		t.Errorf("Subject = %q, want %q", m.Subject, "orders.eu")
	}
	if m.Content != "3 created" {
		t.Errorf("Content = %q, want %q", m.Content, "3 created")
	}
	if len(feed.calls) != 1 || feed.calls[0].topic != "orders.eu" {
		t.Fatalf("expected one PublishFeed call with topic %q, got %+v", "orders.eu", feed.calls)
	}
}

// TestHandleStream_PublishFailureDoesNotFailTask proves a feed publish
// failure is best-effort: the message is already persisted, so the task
// still returns the saved message with a nil error.
func TestHandleStream_PublishFailureDoesNotFailTask(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	feed := &stubFeedPublisher{err: errPublishBoom}
	h := NewStream(s).WithFeed(feed)

	input, err := json.Marshal(streamEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	out, err := h.HandleStream(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input})
	if err != nil {
		t.Fatalf("HandleStream returned error, want nil: %v", err)
	}
	var m domain.Message
	if uerr := json.Unmarshal(out, &m); uerr != nil {
		t.Fatalf("unmarshal handler output: %v", uerr)
	}
	if m.Status != "sent" {
		t.Errorf("Status = %q, want %q", m.Status, "sent")
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "stream")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "sent" {
		t.Fatalf("expected one sent message despite the feed publish failure, got %+v", msgs)
	}
}
