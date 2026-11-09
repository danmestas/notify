package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// verifySMSSubscriber stores user_123 with a verified phone, the only kind an
// sms step sends to.
func verifySMSSubscriber(t *testing.T, s *store.Store, phone string) {
	t.Helper()
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "user_123", FirstName: "Ada"}); err != nil {
		t.Fatalf("upsert subscriber: %v", err)
	}
	if err := s.Subscribers().SetVerifiedPhone(ctx, scope, "user_123", phone, "2026-11-08T12:00:00Z"); err != nil {
		t.Fatalf("SetVerifiedPhone: %v", err)
	}
}

// smsSkipped runs the step and asserts it was skipped for reason, with no
// error and without the provider being called.
func smsSkipped(t *testing.T, s *store.Store, h *SMSHandler, capt *providers.CaptureSMS, env runenv.InAppEnvelope, reason string) {
	t.Helper()
	input, _ := json.Marshal(env)
	if _, err := h.HandleSMS(context.Background(), engine.TaskPayload{RunID: "run_skip", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}
	msgs, err := s.Messages().List(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != "skipped" || msgs[0].SkipReason != reason || msgs[0].ErrorText != "" {
		t.Fatalf("messages = %+v, want one skipped (%s) message with no error", msgs, reason)
	}
	if got := len(capt.Sent()); got != 0 {
		t.Errorf("provider invoked %d times, want 0", got)
	}
}

// TestHandleSMS_NoPhoneIsSkipped: a subscriber with no phone at all is not a
// fault; the step is skipped and the run goes on.
func TestHandleSMS_NoPhoneIsSkipped(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	if _, err := s.Subscribers().Upsert(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, domain.Subscriber{SubscriberID: "user_123"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	env := smsEnvelope()
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_123","firstName":"Ada"}`)
	smsSkipped(t, s, NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}), capt, env, SkipReasonNoVerifiedPhone)
}

// TestHandleSMS_UnverifiedPhoneIsSkipped: a phone the sender wrote through the
// API is never sent to; only the subscriber can verify one.
func TestHandleSMS_UnverifiedPhoneIsSkipped(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	if _, err := s.Subscribers().Upsert(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, domain.Subscriber{SubscriberID: "user_123", Phone: "+15557654321"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	smsSkipped(t, s, NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}), capt, smsEnvelope(), SkipReasonNoVerifiedPhone)
}

// TestHandleSMS_SendsToTheVerifiedPhoneNotTheSnapshot: the recipient is the
// stored verified phone, not whatever phone the trigger-time snapshot holds.
func TestHandleSMS_SendsToTheVerifiedPhoneNotTheSnapshot(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	verifySMSSubscriber(t, s, "+15550009999")
	input, _ := json.Marshal(smsEnvelope()) // the snapshot carries +15557654321
	if _, err := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).HandleSMS(context.Background(), engine.TaskPayload{RunID: "r", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}
	if sent := capt.Sent(); len(sent) != 1 || sent[0].To != "+15550009999" {
		t.Fatalf("sent = %+v, want one sms to the verified phone", sent)
	}
}

// TestHandleSMS_PreferenceSkipCarriesItsReason: a skip by preference says so,
// like every other channel's.
func TestHandleSMS_PreferenceSkipCarriesItsReason(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	verifySMSSubscriber(t, s, "+15557654321")
	h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).WithPreferences(stubChecker{enabled: false})
	smsSkipped(t, s, h, capt, smsEnvelope(), SkipReasonPreference)
}

// defaultChecker answers like preferences.Service for a subscriber with no
// stored preference: the workflow's default decides.
type defaultChecker struct{}

func (defaultChecker) ChannelEnabled(_ context.Context, _ domain.Scope, _, _, _ string, critical, defaultOn bool) (bool, error) {
	return critical || defaultOn, nil
}

// TestHandleSMS_WorkflowDefault: with no preference an sms step is off unless
// its workflow turns it on.
func TestHandleSMS_WorkflowDefault(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		s := newStore(t)
		capt := providers.NewCaptureSMS()
		verifySMSSubscriber(t, s, "+15557654321")
		h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).WithPreferences(defaultChecker{})
		smsSkipped(t, s, h, capt, smsEnvelope(), SkipReasonPreference)
	})
	t.Run("on where the workflow says so", func(t *testing.T) {
		s := newStore(t)
		capt := providers.NewCaptureSMS()
		verifySMSSubscriber(t, s, "+15557654321")
		h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).WithPreferences(defaultChecker{})
		env := smsEnvelope()
		env.Step.DefaultEnabled = true
		input, _ := json.Marshal(env)
		if _, err := h.HandleSMS(context.Background(), engine.TaskPayload{RunID: "r", StepID: "S1", Input: input}); err != nil {
			t.Fatalf("HandleSMS: %v", err)
		}
		if got := len(capt.Sent()); got != 1 {
			t.Fatalf("sent %d, want 1", got)
		}
	})
}
