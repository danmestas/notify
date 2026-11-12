package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestSMSConsent_SettingsDefaultOff: an environment starts with operator
// attestation off and no policy URL; Set and Get round-trip.
func TestSMSConsent_SettingsDefaultOff(t *testing.T) {
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
	got, err := s.SMSConsents().Settings(ctx, scope)
	if err != nil || got.Enabled || got.PolicyURL != "" {
		t.Fatalf("default settings = %+v, %v; want off with no policy", got, err)
	}
	want := SMSConsentSettings{Enabled: true, PolicyURL: "https://shop.example/privacy"}
	if err := s.SMSConsents().SetSettings(ctx, scope, want); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if got, _ := s.SMSConsents().Settings(ctx, scope); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	if _, err := s.SMSConsents().Settings(ctx, domain.Scope{OrgID: org.ID, EnvID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing env: %v, want ErrNotFound", err)
	}
}

// TestSMSConsent_AttestRecordsAndClears: attesting sets the subscriber's
// phone and marks it attested, with an audit row; a phone change, a revoke
// and a soft delete each clear it, and every audit row is kept with the time
// it stopped counting.
func TestSMSConsent_AttestRecordsAndClears(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	subs, consents := s.Subscribers(), s.SMSConsents()
	if _, err := subs.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+15550000000"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	a, err := consents.Attest(ctx, scope, SMSAttestation{
		SubscriberID: "sub-1", Phone: "+15551112222", APIKeyID: "keyhash",
		Source: "signed up in store", ConsentedAt: "2026-01-02T00:00:00Z", PolicyURL: "https://shop.example/privacy",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if a.ID == "" || a.CreatedAt == "" || a.RevokedAt != "" {
		t.Fatalf("attestation = %+v", a)
	}
	got, _ := subs.GetByExternalID(ctx, scope, "sub-1")
	if got.Phone != "+15551112222" || got.PhoneAttestedAt == "" || got.PhoneVerifiedAt != "" {
		t.Fatalf("after attest a new number: %+v, want it attested", got)
	}
	if got.PhoneConsent() != domain.PhoneConsentOperatorAttested {
		t.Fatalf("consent = %q", got.PhoneConsent())
	}

	// The same number in another format is no change.
	got, _ = subs.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+1 (555) 111-2222"})
	if got.PhoneAttestedAt == "" {
		t.Fatalf("same number cleared the attestation: %+v", got)
	}
	// A different number clears it and closes the audit row.
	got, _ = subs.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+15553334444"})
	if got.PhoneAttestedAt != "" || got.PhoneConsent() != domain.PhoneConsentNone {
		t.Fatalf("after a phone change: %+v, want no consent", got)
	}
	rows, err := consents.List(ctx, scope, "sub-1")
	if err != nil || len(rows) != 1 || rows[0].RevokedAt == "" || rows[0].RevokedReason != RevokedPhoneChanged {
		t.Fatalf("audit = %+v, %v; want one row revoked for phone_changed", rows, err)
	}
	if rows[0].APIKeyID != "keyhash" || rows[0].PolicyURL != "https://shop.example/privacy" ||
		rows[0].Source != "signed up in store" || rows[0].ConsentedAt != "2026-01-02T00:00:00Z" || rows[0].Phone != "+15551112222" {
		t.Fatalf("audit row = %+v", rows[0])
	}

	// Revoke clears an attestation; a second revoke has nothing to clear.
	if _, err := consents.Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15553334444", APIKeyID: "k"}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	n, err := consents.Revoke(ctx, scope, "sub-1", "revoker", "customer asked by phone")
	if err != nil || n != 1 {
		t.Fatalf("Revoke = %d, %v; want 1", n, err)
	}
	if got, _ := subs.GetByExternalID(ctx, scope, "sub-1"); got.PhoneAttestedAt != "" || got.Phone != "+15553334444" {
		t.Fatalf("after revoke: %+v, want the phone kept without consent", got)
	}
	if n, err := consents.Revoke(ctx, scope, "sub-1", "revoker", ""); err != nil || n != 0 {
		t.Fatalf("second Revoke = %d, %v; want 0", n, err)
	}

	// Soft delete clears it too.
	if _, err := consents.Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15553334444", APIKeyID: "k"}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if err := subs.SoftDelete(ctx, scope, got.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	rows, _ = consents.List(ctx, scope, "sub-1")
	if len(rows) != 3 {
		t.Fatalf("audit rows = %d, want 3 kept", len(rows))
	}
	reasons := []string{rows[0].RevokedReason, rows[1].RevokedReason, rows[2].RevokedReason}
	if reasons[0] != RevokedPhoneChanged || reasons[1] != RevokedByAPI || reasons[2] != RevokedSubscriberDeleted {
		t.Fatalf("revoked reasons = %v", reasons)
	}
	// Who revoked: the key and its note for an API revoke; nobody for the
	// automatic ones.
	if rows[1].RevokedByKeyID != "revoker" || rows[1].RevokedNote != "customer asked by phone" {
		t.Fatalf("API revoke row = %+v, want the revoking key and its note", rows[1])
	}
	if rows[0].RevokedByKeyID != "" || rows[2].RevokedByKeyID != "" {
		t.Fatalf("automatic revocations name a key: %+v / %+v", rows[0], rows[2])
	}

	// No live subscriber: not found.
	if _, err := consents.Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15553334444"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attest a deleted subscriber: %v, want ErrNotFound", err)
	}
	if _, err := consents.Revoke(ctx, scope, "ghost", "k", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: %v, want ErrNotFound", err)
	}
}

// TestSMSConsent_SelfVerifiedNumberIsNotReplaced: attesting a number other
// than the one the subscriber verified is refused and changes nothing;
// attesting the verified number itself succeeds and is recorded, and the
// phone still reads as self_verified.
func TestSMSConsent_SelfVerifiedNumberIsNotReplaced(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	subs := s.Subscribers()
	if _, err := subs.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := subs.SetVerifiedPhone(ctx, scope, "sub-1", "+15551112222", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := s.SMSConsents().Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15559998888", APIKeyID: "k"}); !errors.Is(err, ErrPhoneSelfVerified) {
		t.Fatalf("attest over a different verified number: %v, want ErrPhoneSelfVerified", err)
	}
	got, _ := subs.GetByExternalID(ctx, scope, "sub-1")
	if got.Phone != "+15551112222" || got.PhoneVerifiedAt == "" || got.PhoneAttestedAt != "" {
		t.Fatalf("refused attest changed the subscriber: %+v", got)
	}
	if rows, _ := s.SMSConsents().List(ctx, scope, "sub-1"); len(rows) != 0 {
		t.Fatalf("refused attest left audit rows: %+v", rows)
	}

	if _, err := s.SMSConsents().Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15551112222", APIKeyID: "k"}); err != nil {
		t.Fatalf("attest the verified number: %v", err)
	}
	got, _ = subs.GetByExternalID(ctx, scope, "sub-1")
	if got.PhoneConsent() != domain.PhoneConsentSelfVerified || got.PhoneVerifiedAt == "" {
		t.Fatalf("same number: %+v (%s), want still self_verified", got, got.PhoneConsent())
	}
	if rows, _ := s.SMSConsents().List(ctx, scope, "sub-1"); len(rows) != 1 || rows[0].RevokedAt != "" {
		t.Fatalf("same-number attest audit = %+v, want one open row", rows)
	}

	// Verifying a different number clears the attestation of the old one.
	if err := subs.SetVerifiedPhone(ctx, scope, "sub-1", "+15559998888", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got, _ = subs.GetByExternalID(ctx, scope, "sub-1"); got.PhoneAttestedAt != "" {
		t.Fatalf("a different verified number kept the attestation: %+v", got)
	}
	rows, _ := s.SMSConsents().List(ctx, scope, "sub-1")
	if len(rows) != 1 || rows[0].RevokedReason != RevokedPhoneChanged {
		t.Fatalf("audit = %+v", rows)
	}
}

// TestSMSConsent_PhoneChangeAndAuditCloseTogether: the subscriber write that
// clears an attestation and the audit row that records it commit together.
// With the audit table gone the write fails and the subscriber is untouched,
// on every path that clears consent.
func TestSMSConsent_PhoneChangeAndAuditCloseTogether(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	setup := func(t *testing.T) (*Store, domain.Subscriber) {
		t.Helper()
		s := openTemp(t)
		sub, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1"})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := s.SMSConsents().Attest(ctx, scope, SMSAttestation{SubscriberID: "sub-1", Phone: "+15551112222", APIKeyID: "k"}); err != nil {
			t.Fatalf("Attest: %v", err)
		}
		if _, err := s.writer.ExecContext(ctx, `DROP TABLE sms_consent_attestations`); err != nil {
			t.Fatalf("drop: %v", err)
		}
		return s, sub
	}
	unchanged := func(t *testing.T, s *Store) {
		t.Helper()
		got, err := s.Subscribers().GetByExternalID(ctx, scope, "sub-1")
		if err != nil || got.Phone != "+15551112222" || got.PhoneAttestedAt == "" || got.Deleted {
			t.Fatalf("subscriber after a failed write = %+v, %v; want it untouched", got, err)
		}
	}
	t.Run("upsert", func(t *testing.T) {
		s, _ := setup(t)
		if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+15553334444"}); err == nil {
			t.Fatal("upsert succeeded without its audit write")
		}
		unchanged(t, s)
	})
	t.Run("verify", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.Subscribers().SetVerifiedPhone(ctx, scope, "sub-1", "+15553334444", "2026-01-01T00:00:00Z"); err == nil {
			t.Fatal("verify succeeded without its audit write")
		}
		unchanged(t, s)
	})
	t.Run("soft delete", func(t *testing.T) {
		s, sub := setup(t)
		if err := s.Subscribers().SoftDelete(ctx, scope, sub.ID); err == nil {
			t.Fatal("soft delete succeeded without its audit write")
		}
		unchanged(t, s)
	})
}

// TestSMSConsent_TenancyIsolation: another environment's subscriber cannot be
// attested or revoked, and its audit rows are not listed.
func TestSMSConsent_TenancyIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if _, err := s.Subscribers().Upsert(ctx, a, domain.Subscriber{SubscriberID: "sub-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.SMSConsents().Attest(ctx, a, SMSAttestation{SubscriberID: "sub-1", Phone: "+15551112222", APIKeyID: "k"}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if _, err := s.SMSConsents().Attest(ctx, b, SMSAttestation{SubscriberID: "sub-1", Phone: "+15551112222"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env attest: %v, want ErrNotFound", err)
	}
	if _, err := s.SMSConsents().Revoke(ctx, b, "sub-1", "k", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env revoke: %v, want ErrNotFound", err)
	}
	if rows, _ := s.SMSConsents().List(ctx, b, "sub-1"); len(rows) != 0 {
		t.Fatalf("cross-env list = %+v", rows)
	}
}

// TestApiKey_AttestPermission: a key is created without the permission, and
// it can be granted and withdrawn.
func TestApiKey_AttestPermission(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if _, err := s.ApiKeys().Create(ctx, scope, domain.APIKey{Hash: "h1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	k, _, err := s.ApiKeys().GetByHash(ctx, "h1")
	if err != nil || k.AttestSMSConsent {
		t.Fatalf("new key = %+v, %v; want no permission", k, err)
	}
	if err := s.ApiKeys().SetAttestSMSConsent(ctx, scope, true); err != nil {
		t.Fatalf("SetAttestSMSConsent: %v", err)
	}
	if k, _, _ := s.ApiKeys().GetByHash(ctx, "h1"); !k.AttestSMSConsent {
		t.Fatalf("granted key = %+v", k)
	}
	if keys, _ := s.ApiKeys().List(ctx, scope); len(keys) != 1 || !keys[0].AttestSMSConsent {
		t.Fatalf("List = %+v", keys)
	}
	if _, err := s.ApiKeys().Create(ctx, scope, domain.APIKey{Hash: "h2", AttestSMSConsent: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if k, _, _ := s.ApiKeys().GetByHash(ctx, "h2"); !k.AttestSMSConsent {
		t.Fatalf("key minted with the permission = %+v", k)
	}
}
