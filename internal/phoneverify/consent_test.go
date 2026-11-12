package phoneverify_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
	"github.com/danmestas/notify/internal/store"
)

// consentFixture is a real environment (attestation needs its setting) with
// one subscriber.
func consentFixture(t *testing.T) (*store.Store, *phoneverify.Service, domain.Scope) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	org, err := st.Organizations().Create(ctx, "Shop")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	_, scope, err := st.Environments().Create(ctx, org.ID, domain.Environment{Name: "prod", Identifier: "prod"})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if _, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "cust-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return st, phoneverify.New(st, nil).WithClock(func() time.Time { return now }), scope
}

var allowed = phoneverify.Attester{KeyID: "keyhash", MayAttest: true}

func TestSetConsentSettings_RequiresAnHTTPSPolicy(t *testing.T) {
	_, svc, scope := consentFixture(t)
	ctx := context.Background()
	for _, bad := range []string{"", "http://shop.example/privacy", "shop.example/privacy", "https://", "https:///privacy", "ftp://x"} {
		if err := svc.SetConsentSettings(ctx, scope, true, bad); !errors.Is(err, phoneverify.ErrPolicyURLRequired) {
			t.Errorf("enable with %q: %v, want ErrPolicyURLRequired", bad, err)
		}
	}
	if err := svc.SetConsentSettings(ctx, scope, true, " https://shop.example/privacy "); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, err := svc.ConsentSettings(ctx, scope)
	if err != nil || !got.Enabled || got.PolicyURL != "https://shop.example/privacy" {
		t.Fatalf("settings = %+v, %v", got, err)
	}
	// Turning it off keeps the URL.
	if err := svc.SetConsentSettings(ctx, scope, false, got.PolicyURL); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got, _ := svc.ConsentSettings(ctx, scope); got.Enabled || got.PolicyURL != "https://shop.example/privacy" {
		t.Fatalf("after disable = %+v", got)
	}
}

func TestAttest_Refusals(t *testing.T) {
	_, svc, scope := consentFixture(t)
	ctx := context.Background()

	if _, err := svc.Attest(ctx, scope, allowed, phoneverify.AttestInput{SubscriberID: "cust-1", Phone: "+15551112222"}); !errors.Is(err, phoneverify.ErrAttestationDisabled) {
		t.Fatalf("setting off: %v, want ErrAttestationDisabled", err)
	}
	if err := svc.SetConsentSettings(ctx, scope, true, "https://shop.example/privacy"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := svc.Attest(ctx, scope, phoneverify.Attester{KeyID: "k"}, phoneverify.AttestInput{SubscriberID: "cust-1", Phone: "+15551112222"}); !errors.Is(err, phoneverify.ErrAttestNotPermitted) {
		t.Fatalf("unpermitted key: %v, want ErrAttestNotPermitted", err)
	}
	if _, err := svc.Attest(ctx, scope, allowed, phoneverify.AttestInput{SubscriberID: "cust-1", Phone: "555"}); !errors.Is(err, phoneverify.ErrInvalidPhone) {
		t.Fatalf("bad phone: %v, want ErrInvalidPhone", err)
	}
	if _, err := svc.Attest(ctx, scope, allowed, phoneverify.AttestInput{SubscriberID: "ghost", Phone: "+15551112222"}); !errors.Is(err, phoneverify.ErrUnknownSubscriber) {
		t.Fatalf("unknown subscriber: %v, want ErrUnknownSubscriber", err)
	}
	for _, when := range []string{"yesterday", "2026-11-13T12:00:00Z"} {
		if _, err := svc.Attest(ctx, scope, allowed, phoneverify.AttestInput{SubscriberID: "cust-1", Phone: "+15551112222", AttestedAt: when}); !errors.Is(err, phoneverify.ErrInvalidAttestedAt) {
			t.Errorf("attestedAt %q: %v, want ErrInvalidAttestedAt", when, err)
		}
	}
}

func TestAttest_RecordsAndSends(t *testing.T) {
	st, svc, scope := consentFixture(t)
	ctx := context.Background()
	if err := svc.SetConsentSettings(ctx, scope, true, "https://shop.example/privacy"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	a, err := svc.Attest(ctx, scope, allowed, phoneverify.AttestInput{
		SubscriberID: "cust-1", Phone: "+1 (555) 111-2222", Source: " opted in at the counter ", AttestedAt: "2025-06-01T10:00:00-05:00",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if a.Phone != "+15551112222" || a.APIKeyID != "keyhash" || a.PolicyURL != "https://shop.example/privacy" ||
		a.Source != "opted in at the counter" || a.ConsentedAt == "" {
		t.Fatalf("attestation = %+v", a)
	}
	sub, _ := st.Subscribers().GetByExternalID(ctx, scope, "cust-1")
	if sub.PhoneConsent() != domain.PhoneConsentOperatorAttested {
		t.Fatalf("consent = %q", sub.PhoneConsent())
	}
	if to, err := phoneverify.SendablePhone(ctx, st, scope, sub); err != nil || to != "+15551112222" {
		t.Fatalf("SendablePhone = %q, %v", to, err)
	}

	// Turning the setting off suspends the attestation.
	if err := svc.SetConsentSettings(ctx, scope, false, "https://shop.example/privacy"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if to, err := phoneverify.SendablePhone(ctx, st, scope, sub); err != nil || to != "" {
		t.Fatalf("SendablePhone with the setting off = %q, %v; want none", to, err)
	}

	// A self-verified phone is sendable whatever the setting.
	if err := st.Subscribers().SetVerifiedPhone(ctx, scope, "cust-1", "+15551112222", "2026-11-12T12:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	sub, _ = st.Subscribers().GetByExternalID(ctx, scope, "cust-1")
	if to, err := phoneverify.SendablePhone(ctx, st, scope, sub); err != nil || to != "+15551112222" {
		t.Fatalf("SendablePhone self-verified = %q, %v", to, err)
	}
}

func TestRevokeAttestation(t *testing.T) {
	st, svc, scope := consentFixture(t)
	ctx := context.Background()
	if err := svc.SetConsentSettings(ctx, scope, true, "https://shop.example/privacy"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := svc.Attest(ctx, scope, allowed, phoneverify.AttestInput{SubscriberID: "cust-1", Phone: "+15551112222"}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if err := svc.RevokeAttestation(ctx, scope, "cust-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	sub, _ := st.Subscribers().GetByExternalID(ctx, scope, "cust-1")
	if to, _ := phoneverify.SendablePhone(ctx, st, scope, sub); to != "" {
		t.Fatalf("revoked phone still sendable: %q", to)
	}
	if err := svc.RevokeAttestation(ctx, scope, "ghost"); !errors.Is(err, phoneverify.ErrUnknownSubscriber) {
		t.Fatalf("revoke unknown: %v", err)
	}
}
