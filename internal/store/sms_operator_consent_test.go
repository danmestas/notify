package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestSMSConsent_SetOperatorConsentIsOneUnit: the environment setting and the
// key permission change together, and an unknown environment changes nothing.
func TestSMSConsent_SetOperatorConsentIsOneUnit(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	org, err := s.Organizations().Create(ctx, "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	_, scope, err := s.Environments().Create(ctx, org.ID, domain.Environment{Name: "dev", Identifier: "dev"})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if _, err := s.ApiKeys().Create(ctx, scope, domain.APIKey{Hash: "h1", EncKey: []byte("x")}); err != nil {
		t.Fatalf("key: %v", err)
	}
	on := SMSConsentSettings{Enabled: true, PolicyURL: "https://shop.example/privacy"}
	if err := s.SMSConsents().SetOperatorConsent(ctx, scope, on); err != nil {
		t.Fatalf("on: %v", err)
	}
	if got, _ := s.SMSConsents().Settings(ctx, scope); got != on {
		t.Fatalf("settings = %+v, want %+v", got, on)
	}
	if keys, _ := s.ApiKeys().List(ctx, scope); len(keys) != 1 || !keys[0].AttestSMSConsent {
		t.Fatalf("key permission not granted: %+v", keys)
	}
	off := SMSConsentSettings{Enabled: false, PolicyURL: on.PolicyURL}
	if err := s.SMSConsents().SetOperatorConsent(ctx, scope, off); err != nil {
		t.Fatalf("off: %v", err)
	}
	if got, _ := s.SMSConsents().Settings(ctx, scope); got != off {
		t.Fatalf("settings = %+v, want %+v", got, off)
	}
	if keys, _ := s.ApiKeys().List(ctx, scope); keys[0].AttestSMSConsent {
		t.Fatal("key permission not withdrawn")
	}
	if err := s.SMSConsents().SetOperatorConsent(ctx, domain.Scope{OrgID: org.ID, EnvID: "nope"}, on); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing env: %v, want ErrNotFound", err)
	}
}
