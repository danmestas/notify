package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
)

// attestSMSSubscriber stores user_123 with an operator-attested phone, in an
// environment whose attestation setting is enabled or not.
func attestSMSSubscriber(t *testing.T, s *store.Store, phone string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	if _, _, err := s.Environments().Create(ctx, "org_1", domain.Environment{ID: "env_1", Name: "prod", Identifier: "prod"}); err != nil {
		t.Fatalf("env: %v", err)
	}
	if err := s.SMSConsents().SetSettings(ctx, scope, store.SMSConsentSettings{Enabled: enabled, PolicyURL: "https://shop.example/privacy"}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "user_123", FirstName: "Ada"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.SMSConsents().Attest(ctx, scope, store.SMSAttestation{SubscriberID: "user_123", Phone: phone, APIKeyID: "k"}); err != nil {
		t.Fatalf("attest: %v", err)
	}
}

// TestHandleSMS_SendsToAnAttestedPhone: an operator-attested phone counts as
// consented while the environment allows attestation.
func TestHandleSMS_SendsToAnAttestedPhone(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	attestSMSSubscriber(t, s, "+15550009999", true)
	input, _ := json.Marshal(smsEnvelope())
	if _, err := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}).HandleSMS(context.Background(), engine.TaskPayload{RunID: "r", StepID: "S1", Input: input}); err != nil {
		t.Fatalf("HandleSMS: %v", err)
	}
	if sent := capt.Sent(); len(sent) != 1 || sent[0].To != "+15550009999" {
		t.Fatalf("sent = %+v, want one sms to the attested phone", sent)
	}
}

// TestHandleSMS_AttestedPhoneNeedsTheSetting: with attestation turned off in
// the environment, an attested phone is skipped like an unverified one.
func TestHandleSMS_AttestedPhoneNeedsTheSetting(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	attestSMSSubscriber(t, s, "+15557654321", false)
	smsSkipped(t, s, NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}), capt, smsEnvelope(), SkipReasonNoVerifiedPhone)
}

// TestHandleSMS_StopBeatsAttestation: a STOP from an attested number skips it
// as opted_out.
func TestHandleSMS_StopBeatsAttestation(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	attestSMSSubscriber(t, s, "+15557654321", true)
	if _, err := s.SMSOptOuts().OptOut(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "+15557654321", "integ-sms-test"); err != nil {
		t.Fatalf("OptOut: %v", err)
	}
	smsSkipped(t, s, NewSMS(s, stubSMSResolver{p: capt, id: "twilio"}), capt, smsEnvelope(), SkipReasonOptedOut)
}
