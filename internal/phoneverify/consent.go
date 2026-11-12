package phoneverify

// Operator-attested consent (#142). A subscriber's own code confirmation is
// one way a phone comes to count as consented to texts. The other is an
// operator, whose customers opted in through their dealings with it under its
// published privacy policy, attesting that consent through the API. It is off
// unless the environment allows it, which needs a privacy policy URL, and only
// an API key granted the permission may attest. Every attestation is kept as
// an audit row naming the key, the time, a source note and the policy URL.
//
// An attestation counts only while its environment allows attestation, and a
// STOP opt-out blocks it like it blocks a self-verified phone. Changing the
// phone, deleting the subscriber, or revoking clears it.

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

const (
	// MaxSourceLen bounds an attestation's source note, and a revocation's
	// reason, in bytes.
	MaxSourceLen = 500
	// maxPolicyURLLen bounds the privacy policy URL.
	maxPolicyURLLen = 2048
	// attestSkew is how far past now an attestedAt may be, for clock skew.
	attestSkew = 5 * time.Minute
)

// earliestAttestedAt is the oldest attestedAt accepted: anything earlier is
// a placeholder or a mistake, not a record of when consent was given.
var earliestAttestedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var (
	// ErrAttestationDisabled: the environment does not allow operators to
	// attest consent.
	ErrAttestationDisabled = errors.New("phoneverify: this environment does not allow operators to attest SMS consent")
	// ErrAttestNotPermitted: the API key was not granted the permission.
	ErrAttestNotPermitted = errors.New("phoneverify: this API key may not attest SMS consent")
	// ErrPolicyURLRequired: allowing attestation needs an https privacy
	// policy URL.
	ErrPolicyURLRequired = errors.New("phoneverify: allowing operator-attested consent needs an https privacy policy URL")
	// ErrInvalidAttestedAt: attestedAt is not RFC 3339, is before
	// earliestAttestedAt, or is in the future.
	ErrInvalidAttestedAt = errors.New("phoneverify: attestedAt must be an RFC 3339 time from 2000-01-01 on, not in the future")
	// ErrSourceTooLong: the source note is over MaxSourceLen.
	ErrSourceTooLong = errors.New("phoneverify: source is too long")
	// ErrReasonTooLong: a revocation's reason is over MaxSourceLen.
	ErrReasonTooLong = errors.New("phoneverify: reason is too long")
	// ErrPhoneSelfVerified: the subscriber verified a different number
	// themselves; an attestation may not replace consent they proved.
	ErrPhoneSelfVerified = errors.New("phoneverify: the subscriber verified a different phone themselves; attest that number, or have them verify the new one")
)

// ConsentSettings is an environment's operator-attestation setting.
type ConsentSettings = store.SMSConsentSettings

// Attestation is one recorded attestation.
type Attestation = store.SMSAttestation

// Attester is the API key attesting: its id and whether it was granted the
// permission.
type Attester struct {
	KeyID     string
	MayAttest bool
}

// AttestInput is what the operator attests.
type AttestInput struct {
	SubscriberID string
	// Phone is normalised to E.164.
	Phone string
	// Source is a free-text note on where consent came from. Optional.
	Source string
	// AttestedAt is when the operator says consent was given, RFC 3339.
	// Optional.
	AttestedAt string
}

// ConsentSettings reads the environment's setting.
func (s *Service) ConsentSettings(ctx context.Context, scope domain.Scope) (ConsentSettings, error) {
	return s.store.SMSConsents().Settings(ctx, scope)
}

// SetConsentSettings allows or stops operator-attested consent in the
// environment. Allowing it needs an https privacy policy URL; a URL given
// while turning it off is kept for next time and checked the same way when
// non-empty. Turning it off suspends existing attestations without clearing
// them.
func (s *Service) SetConsentSettings(ctx context.Context, scope domain.Scope, enabled bool, policyURL string) error {
	policyURL = strings.TrimSpace(policyURL)
	if (enabled || policyURL != "") && !validPolicyURL(policyURL) {
		return ErrPolicyURLRequired
	}
	return s.store.SMSConsents().SetSettings(ctx, scope, ConsentSettings{Enabled: enabled, PolicyURL: policyURL})
}

// Attest records that the subscriber consented to texts at in.Phone. The
// subscriber's phone becomes that number, and the attestation replaces any
// earlier one. ErrPhoneSelfVerified when the subscriber verified a different
// number themselves; attesting the number they verified is recorded and
// leaves it self-verified.
func (s *Service) Attest(ctx context.Context, scope domain.Scope, by Attester, in AttestInput) (Attestation, error) {
	settings, err := s.ConsentSettings(ctx, scope)
	if err != nil {
		return Attestation{}, err
	}
	if !settings.Enabled {
		return Attestation{}, ErrAttestationDisabled
	}
	if !by.MayAttest || by.KeyID == "" {
		return Attestation{}, ErrAttestNotPermitted
	}
	phone, err := Normalize(in.Phone)
	if err != nil {
		return Attestation{}, err
	}
	source := strings.TrimSpace(in.Source)
	if len(source) > MaxSourceLen {
		return Attestation{}, ErrSourceTooLong
	}
	consentedAt := ""
	if in.AttestedAt != "" {
		t, err := time.Parse(time.RFC3339, in.AttestedAt)
		if err != nil || t.Before(earliestAttestedAt) || t.After(s.now().Add(attestSkew)) {
			return Attestation{}, ErrInvalidAttestedAt
		}
		consentedAt = timestamp.Format(t)
	}
	a, err := s.store.SMSConsents().Attest(ctx, scope, store.SMSAttestation{
		SubscriberID: in.SubscriberID, Phone: phone, APIKeyID: by.KeyID,
		Source: source, ConsentedAt: consentedAt, PolicyURL: settings.PolicyURL,
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Attestation{}, ErrUnknownSubscriber
	case errors.Is(err, store.ErrPhoneSelfVerified):
		return Attestation{}, ErrPhoneSelfVerified
	}
	return a, err
}

// RevokeAttestation clears the subscriber's operator attestation, keeping its
// audit row with the revoking key and reason. A self-verified phone stays
// verified. Revoking where there is no attestation is not an error.
func (s *Service) RevokeAttestation(ctx context.Context, scope domain.Scope, keyID, subscriberID, reason string) error {
	reason = strings.TrimSpace(reason)
	if len(reason) > MaxSourceLen {
		return ErrReasonTooLong
	}
	_, err := s.store.SMSConsents().Revoke(ctx, scope, subscriberID, keyID, reason)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnknownSubscriber
	}
	return err
}

// SendablePhone is the phone an sms step may text for sub, or "" when it has
// no consented phone: none, a deleted subscriber, neither self-verified nor
// attested, or attested while the environment no longer allows attestation.
// Opt-outs are the caller's to check: STOP blocks either kind.
func SendablePhone(ctx context.Context, st *store.Store, scope domain.Scope, sub domain.Subscriber) (string, error) {
	if sub.Deleted {
		return "", nil
	}
	switch sub.PhoneConsent() {
	case domain.PhoneConsentSelfVerified:
		return sub.Phone, nil
	case domain.PhoneConsentOperatorAttested:
		settings, err := st.SMSConsents().Settings(ctx, scope)
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if settings.Enabled {
			return sub.Phone, nil
		}
	}
	return "", nil
}

func validPolicyURL(raw string) bool {
	if raw == "" || len(raw) > maxPolicyURLLen {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.User == nil
}
