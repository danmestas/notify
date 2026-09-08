package identity

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

var sessionScope = domain.Scope{OrgID: "org_1", EnvID: "env_1"}

func TestSubscriberSession_RoundTrip(t *testing.T) {
	svc := newService(t)

	token, expiresAt := svc.MintSubscriberSession(sessionScope, "alice")
	claims, err := svc.VerifySubscriberSession(token)
	if err != nil {
		t.Fatalf("VerifySubscriberSession: %v", err)
	}
	if claims.SubscriberID != "alice" {
		t.Errorf("SubscriberID = %q, want alice", claims.SubscriberID)
	}
	if claims.Scope != sessionScope {
		t.Errorf("Scope = %+v, want %+v", claims.Scope, sessionScope)
	}
	if got := claims.ExpiresAt.Unix(); got != expiresAt.Unix() {
		t.Errorf("ExpiresAt = %v, want %v", claims.ExpiresAt, expiresAt)
	}
	if time.Until(expiresAt) > SessionTTL+time.Minute {
		t.Errorf("token outlives SessionTTL: %v", expiresAt)
	}
}

// TestSubscriberSession_Rejects covers every way a token must fail closed. A
// token that verifies when it should not is a cross-subscriber inbox leak.
func TestSubscriberSession_Rejects(t *testing.T) {
	svc := newService(t)
	valid, _ := svc.MintSubscriberSession(sessionScope, "alice")
	payload, mac, _ := strings.Cut(valid, ".")

	// Re-sign a different subscriber under a DIFFERENT key: the shape is
	// perfect, only the signing key is wrong.
	other := newService(t)
	forged, _ := other.MintSubscriberSession(sessionScope, "alice")

	// A payload edited to name bob, keeping alice's signature.
	tamperedPayload := base64.RawURLEncoding.EncodeToString(
		encodeSessionPayload(sessionScope.EnvID, sessionScope.OrgID, "bob", time.Now().Add(time.Hour)),
	)

	tests := []struct{ name, token string }{
		{"empty", ""},
		{"no separator", payload + mac},
		{"payload only", payload},
		{"mac only", "." + mac},
		{"tampered subscriber", tamperedPayload + "." + mac},
		{"signed with another deployment's key", forged},
		{"truncated mac", payload + "." + mac[:len(mac)-4]},
		{"garbage", "not-a-token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.VerifySubscriberSession(tc.token); err == nil {
				t.Fatalf("token %q verified, want rejection", tc.token)
			}
		})
	}
}

// TestSubscriberSession_ExpiryEnforced: a correctly signed but expired token
// must not authenticate. Expiry is the only thing that ends a stateless token
// early, so it has to be checked.
func TestSubscriberSession_ExpiryEnforced(t *testing.T) {
	svc := newService(t)

	// Mint by hand in the past, signed with the real key.
	past := time.Now().Add(-time.Minute)
	payload := encodeSessionPayload(sessionScope.EnvID, sessionScope.OrgID, "alice", past)
	mac := svc.signSession(payload)
	expired := b64session(payload) + "." + b64session(mac[:sessionMACLen])

	if _, err := svc.VerifySubscriberSession(expired); err == nil {
		t.Fatal("expired token verified, want rejection")
	}
}

// TestSubscriberSession_KeyIsDomainSeparated: session tokens must not be
// signed with the raw master key that seals provider credentials, so that a
// signing oracle can never become a decryption oracle.
func TestSubscriberSession_KeyIsDomainSeparated(t *testing.T) {
	key := testKey(t)
	svc := New(newService(t).store, key)

	if string(svc.sessionKey) == string(key) {
		t.Fatal("session signing key is the raw master key; derive a subkey instead")
	}
	if len(svc.sessionKey) == 0 {
		t.Fatal("session signing key is empty; every token would be forgeable")
	}
}

// TestSubscriberSession_ScopeIsCarried: a token minted in one tenant must not
// authenticate in another, even for the same subscriber id.
func TestSubscriberSession_ScopeIsCarried(t *testing.T) {
	svc := newService(t)

	tokenA, _ := svc.MintSubscriberSession(domain.Scope{OrgID: "org_a", EnvID: "env_a"}, "alice")
	claims, err := svc.VerifySubscriberSession(tokenA)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Scope.OrgID != "org_a" || claims.Scope.EnvID != "env_a" {
		t.Fatalf("scope = %+v, want org_a/env_a — a token must not float between tenants", claims.Scope)
	}
}
