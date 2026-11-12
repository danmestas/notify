package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// keyOf runs key through Middleware and returns the key it attached.
func keyOf(t *testing.T, svc *Service, key string) (Key, bool) {
	t.Helper()
	var (
		got Key
		ok  bool
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, ok = KeyFrom(r.Context()) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "ApiKey "+key)
	svc.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	return got, ok
}

// TestAttestPermission: a key starts without the permission; the console
// grants it, rotation keeps it unless told otherwise, and Middleware tells a
// handler which key called and what it may do.
func TestAttestPermission(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	raw, scope, err := svc.Bootstrap(ctx, "Acme", "prod")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	k, ok := keyOf(t, svc, raw)
	if !ok || k.ID == "" || k.AttestSMSConsent {
		t.Fatalf("new key = %+v (%v), want an id and no permission", k, ok)
	}
	if allowed, err := svc.AttestSMSConsentAllowed(ctx, scope); err != nil || allowed {
		t.Fatalf("AttestSMSConsentAllowed = %v, %v", allowed, err)
	}

	if err := svc.SetAttestSMSConsent(ctx, scope, true); err != nil {
		t.Fatalf("SetAttestSMSConsent: %v", err)
	}
	if k, _ := keyOf(t, svc, raw); !k.AttestSMSConsent {
		t.Fatalf("granted key = %+v", k)
	}

	rotated, err := svc.RotateKey(ctx, scope)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	k2, _ := keyOf(t, svc, rotated)
	if !k2.AttestSMSConsent || k2.ID == k.ID {
		t.Fatalf("rotated key = %+v, want the permission kept under a new id", k2)
	}

	rotated, err = svc.RotateKey(ctx, scope, AllowSMSConsentAttestation(false))
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if k3, _ := keyOf(t, svc, rotated); k3.AttestSMSConsent {
		t.Fatalf("rotated without the permission = %+v", k3)
	}
	rotated, err = svc.RotateKey(ctx, scope, AllowSMSConsentAttestation(true))
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if k4, _ := keyOf(t, svc, rotated); !k4.AttestSMSConsent {
		t.Fatalf("rotated with the permission = %+v", k4)
	}
}
