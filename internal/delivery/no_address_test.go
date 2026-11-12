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

// A recipient with no address for a step's channel is an expected situation,
// not a fault: the step records skipped with a per-channel reason, calls no
// provider, and leaves integration health alone. A missing integration is a
// configuration error of its own class. A real provider failure is unchanged.

func runStep(t *testing.T, run func(context.Context, engine.TaskPayload) (json.RawMessage, error), env runenv.InAppEnvelope) domain.Message {
	t.Helper()
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := run(context.Background(), engine.TaskPayload{RunID: "run_na", StepID: env.Step.StepID, Input: input})
	if err != nil {
		t.Fatalf("handler returned %v, want a recorded message and a nil error", err)
	}
	var m domain.Message
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return m
}

func wantSkipped(t *testing.T, m domain.Message, reason string) {
	t.Helper()
	if m.Status != "skipped" || m.SkipReason != reason || m.ErrorText != "" || m.ErrorClass != "" {
		t.Fatalf("message = status %q skip %q err %q class %q, want skipped/%s with no error", m.Status, m.SkipReason, m.ErrorText, m.ErrorClass, reason)
	}
}

// wantHealthUntouched fails when the integration has any health row: a skip
// or a missing-integration error must never reach the tracker.
func wantHealthUntouched(t *testing.T, s *store.Store, integrationID string) {
	t.Helper()
	_, err := s.IntegrationHealth().Get(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, integrationID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("integration health row exists (err %v), want none: the step must not touch health", err)
	}
}

func TestHandleEmail_NoEmail_SkipsWithoutProviderCallOrHealth(t *testing.T) {
	s, integ := healthRig(t, "resend", "email")
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "a@b.io", providerID: "resend", integrationID: integ})
	env := emailEnvelope()
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_1","email":"","firstName":"Ada"}`)

	wantSkipped(t, runStep(t, h.HandleEmail, env), SkipReasonNoEmail)
	if n := len(capt.Sent()); n != 0 {
		t.Errorf("provider invoked %d times with no email, want 0", n)
	}
	wantHealthUntouched(t, s, integ)
}

func TestHandleEmail_NoEmailAndNoIntegration_SkipsNotErrors(t *testing.T) {
	s := newStore(t)
	h := NewEmail(s, stubResolver{err: store.ErrNotFound})
	env := emailEnvelope()
	env.Subscriber = json.RawMessage(`{"subscriberId":"user_1","firstName":"Ada"}`)
	wantSkipped(t, runStep(t, h.HandleEmail, env), SkipReasonNoEmail)
}

func TestHandleEmail_NoIntegration_IsNotConfiguredAndLeavesHealth(t *testing.T) {
	s, integ := healthRig(t, "resend", "email")
	h := NewEmail(s, stubResolver{err: store.ErrNotFound})

	m := runStep(t, h.HandleEmail, emailEnvelope())
	if m.Status != "error" || m.ErrorClass != string(providers.ClassNotConfigured) {
		t.Fatalf("message = status %q class %q, want error/%s", m.Status, m.ErrorClass, providers.ClassNotConfigured)
	}
	wantHealthUntouched(t, s, integ)
}

func TestHandleEmail_ProviderFailureStaysError(t *testing.T) {
	s, integ := healthRig(t, "resend", "email")
	h := NewEmail(s, stubResolver{provider: refusingProvider{err: errors.New("resend: status 422: bad payload")}, providerID: "resend", integrationID: integ})

	m := runStep(t, h.HandleEmail, emailEnvelope())
	if m.Status != "error" || m.ErrorClass != string(providers.ClassRejectedMessage) || m.IntegrationID != integ {
		t.Fatalf("message = status %q class %q integ %q, want error/rejected_message on the integration", m.Status, m.ErrorClass, m.IntegrationID)
	}
	if _, err := s.IntegrationHealth().Get(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, integ); err != nil {
		t.Errorf("a real provider failure left no health row: %v", err)
	}
}

func TestHandlePush_NoDevice_SkipsWithoutProviderCallOrHealth(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		err  error
	}{
		{"ntfy, no topic", "ntfy", nil},
		{"webpush, no subscriptions", webPushProviderID, nil},
		{"no integration at all", "", store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, integ := healthRig(t, "ntfy", "push")
			capt := providers.NewCapturePush()
			h := NewPush(s, stubPushResolver{p: capt, id: tc.id, integrationID: integ, err: tc.err})

			wantSkipped(t, runStep(t, h.HandlePush, pushEnvelope()), SkipReasonNoDevice)
			if n := len(capt.Sent()); n != 0 {
				t.Errorf("provider invoked %d times with no device, want 0", n)
			}
			wantHealthUntouched(t, s, integ)
		})
	}
}

func TestHandlePush_HasDeviceButNoIntegration_IsNotConfigured(t *testing.T) {
	s, integ := healthRig(t, "ntfy", "push")
	seedTopic(t, s, "agent-alerts")
	h := NewPush(s, stubPushResolver{err: store.ErrNotFound})

	m := runStep(t, h.HandlePush, pushEnvelope())
	if m.Status != "error" || m.ErrorClass != string(providers.ClassNotConfigured) {
		t.Fatalf("message = status %q class %q, want error/%s", m.Status, m.ErrorClass, providers.ClassNotConfigured)
	}
	wantHealthUntouched(t, s, integ)
}

func TestHandlePush_ProviderFailureStaysError(t *testing.T) {
	s, integ := healthRig(t, "ntfy", "push")
	seedTopic(t, s, "agent-alerts")
	h := NewPush(s, stubPushResolver{p: failingPushProvider{err: errors.New("ntfy: status 400: bad")}, id: "ntfy", integrationID: integ})

	m := runStep(t, h.HandlePush, pushEnvelope())
	if m.Status != "error" || m.ErrorClass != string(providers.ClassRejectedMessage) {
		t.Fatalf("message = status %q class %q, want error/rejected_message", m.Status, m.ErrorClass)
	}
}

func TestHandleChat_NoChannel_SkipsWithoutProviderCallOrHealth(t *testing.T) {
	for _, id := range []string{"telegram", "slack", "whatsapp"} {
		t.Run(id, func(t *testing.T) {
			s, integ := healthRig(t, id, "chat")
			capt := providers.NewCaptureChat()
			h := NewChat(s, stubChatResolver{p: capt, id: id, integrationID: integ})

			wantSkipped(t, runStep(t, h.HandleChat, chatEnvelope()), SkipReasonNoChannel)
			if n := len(capt.Sent()); n != 0 {
				t.Errorf("provider invoked %d times with no chat address, want 0", n)
			}
			wantHealthUntouched(t, s, integ)
		})
	}
}

// Discord posts to the integration's webhook, so a subscriber with no chat
// address of their own is still reachable.
func TestHandleChat_DiscordNeedsNoSubscriberAddress(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureChat()
	h := NewChat(s, stubChatResolver{p: capt, id: "discord"})

	if m := runStep(t, h.HandleChat, chatEnvelope()); m.Status != "sent" {
		t.Fatalf("status = %q, want sent", m.Status)
	}
}

func TestHandleChat_NoIntegration_IsNotConfiguredAndLeavesHealth(t *testing.T) {
	s, integ := healthRig(t, "telegram", "chat")
	h := NewChat(s, stubChatResolver{err: store.ErrNotFound})

	m := runStep(t, h.HandleChat, chatEnvelope())
	if m.Status != "error" || m.ErrorClass != string(providers.ClassNotConfigured) {
		t.Fatalf("message = status %q class %q, want error/%s", m.Status, m.ErrorClass, providers.ClassNotConfigured)
	}
	wantHealthUntouched(t, s, integ)
}

func TestHandleSMS_NoIntegration_IsNotConfigured(t *testing.T) {
	s := newStore(t)
	verifySMSSubscriber(t, s, "+15557654321")
	h := NewSMS(s, stubSMSResolver{err: store.ErrNotFound})

	m := runStep(t, h.HandleSMS, smsEnvelope())
	if m.Status != "error" || m.ErrorClass != string(providers.ClassNotConfigured) {
		t.Fatalf("message = status %q class %q, want error/%s", m.Status, m.ErrorClass, providers.ClassNotConfigured)
	}
}

// Every gate that skips a chat or push step records why, so an operator can
// tell a false condition, a broken one and a subscriber's choice apart.
func TestHandleChatAndPush_SkipRecordsGateReason(t *testing.T) {
	falseCond := json.RawMessage(`[{"combinator":"and","conditions":[{"on":"subscriber","field":"plan","operator":"eq","value":"pro"}]}]`)
	broken := json.RawMessage(`{"not":"a list of groups"}`)
	cases := []struct {
		name    string
		filters json.RawMessage
		prefOff bool
		want    string
	}{
		{"condition false", falseCond, false, SkipReasonCondition},
		{"condition invalid", broken, false, SkipReasonConditionInvalid},
		{"preference off", nil, true, SkipReasonPreference},
	}
	for _, tc := range cases {
		t.Run("chat/"+tc.name, func(t *testing.T) {
			s := newStore(t)
			seedChatAddress(t, s, "telegram", "987654321")
			capt := providers.NewCaptureChat()
			h := NewChat(s, stubChatResolver{p: capt, id: "telegram"}).WithPreferences(stubChecker{enabled: !tc.prefOff})
			env := chatEnvelope()
			env.Step.Filters = tc.filters
			wantSkipped(t, runStep(t, h.HandleChat, env), tc.want)
			if n := len(capt.Sent()); n != 0 {
				t.Errorf("provider invoked %d times on skip, want 0", n)
			}
		})
		t.Run("push/"+tc.name, func(t *testing.T) {
			s := newStore(t)
			seedTopic(t, s, "agent-alerts")
			capt := providers.NewCapturePush()
			h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"}).WithPreferences(stubChecker{enabled: !tc.prefOff})
			env := pushEnvelope()
			env.Step.Filters = tc.filters
			wantSkipped(t, runStep(t, h.HandlePush, env), tc.want)
			if n := len(capt.Sent()); n != 0 {
				t.Errorf("provider invoked %d times on skip, want 0", n)
			}
		})
	}
}
