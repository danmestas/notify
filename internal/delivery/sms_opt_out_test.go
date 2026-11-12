package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
)

// TestHandleSMS_StoresTheProviderMessageID: the id the provider returned is
// kept on the sent message, so a delivery-status callback can find it.
func TestHandleSMS_StoresTheProviderMessageID(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	verifySMSSubscriber(t, s, "+15557654321")
	input, _ := json.Marshal(smsEnvelope())
	out, err := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).HandleSMS(context.Background(), engine.TaskPayload{RunID: "r", StepID: "S1", Input: input})
	if err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}
	var m domain.Message
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Status != "sent" || m.ProviderMessageID != "capture-sms-1" {
		t.Fatalf("message = %+v, want sent with the provider's id", m)
	}
	got, err := s.Messages().GetByProviderMessageID(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, m.IntegrationID, "capture-sms-1")
	if err != nil || got.ID != m.ID {
		t.Fatalf("lookup by provider id = %+v, %v", got, err)
	}
}

// TestHandleSMS_StatusCallback: with notify's public URL known, every send
// asks the provider to report delivery status to the integration's status
// route; without it, none is asked for.
func TestHandleSMS_StatusCallback(t *testing.T) {
	for name, c := range map[string]struct{ base, want string }{
		"public url":    {"https://notify.example.com/base", "https://notify.example.com/base/v1/inbound/sms/integ-sms-test/status"},
		"no public url": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			capt := providers.NewCaptureSMS()
			verifySMSSubscriber(t, s, "+15557654321")
			input, _ := json.Marshal(smsEnvelope())
			h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).WithStatusCallbackBase(c.base)
			if _, err := h.HandleSMS(context.Background(), engine.TaskPayload{RunID: "r", StepID: "S1", Input: input}); err != nil {
				t.Fatalf("HandleSMS: %v", err)
			}
			if sent := capt.Sent(); len(sent) != 1 || sent[0].StatusCallback != c.want {
				t.Fatalf("sent = %+v, want StatusCallback %q", sent, c.want)
			}
		})
	}
}

// TestHandleSMS_OptedOutIsSkipped: a number that texted STOP gets nothing,
// skipped as opted_out (not preference), even where the subscriber's own
// preference or a critical workflow would send.
func TestHandleSMS_OptedOutIsSkipped(t *testing.T) {
	for name, env := range map[string]func() []byte{
		"default workflow": func() []byte { b, _ := json.Marshal(smsEnvelope()); return b },
		"critical workflow": func() []byte {
			e := smsEnvelope()
			e.WorkflowCritical = true
			b, _ := json.Marshal(e)
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			capt := providers.NewCaptureSMS()
			verifySMSSubscriber(t, s, "+15557654321")
			if _, err := s.SMSOptOuts().OptOut(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "+15557654321", "integ-sms-test"); err != nil {
				t.Fatalf("OptOut: %v", err)
			}
			h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).WithPreferences(stubChecker{enabled: true})
			if _, err := h.HandleSMS(context.Background(), engine.TaskPayload{RunID: "r", StepID: "S1", Input: env()}); err != nil {
				t.Fatalf("HandleSMS: %v", err)
			}
			msgs, _ := s.Messages().List(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "sms")
			if len(msgs) != 1 || msgs[0].Status != "skipped" || msgs[0].SkipReason != SkipReasonOptedOut {
				t.Fatalf("messages = %+v, want one skipped (opted_out)", msgs)
			}
			if len(capt.Sent()) != 0 {
				t.Fatal("an opted-out number was sent to")
			}
		})
	}
}
