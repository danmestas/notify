package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func newService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, testKey(t))
}

func TestNewRejectsWrongKeyLen(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New with %d-byte key: expected panic", n)
				}
			}()
			New(nil, make([]byte, n))
		}()
	}
}

func TestBootstrapThenAuthenticate(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	key, scope, err := svc.Bootstrap(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if scope.OrgID == "" || scope.EnvID == "" {
		t.Fatalf("Bootstrap returned empty scope: %+v", scope)
	}

	got, err := svc.Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != scope {
		t.Fatalf("Authenticate scope = %+v, want %+v", got, scope)
	}
}

func TestAuthenticateWrongKey(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Bootstrap(ctx, "Acme", "production"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "deadbeef"); err == nil {
		t.Fatal("Authenticate wrong key: expected error, got nil")
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	svc := newService(t)
	raw := make([]byte, keyLen)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	enc, err := svc.encrypt(raw)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(enc, raw) {
		t.Fatal("ciphertext equals plaintext")
	}
	if len(enc) <= svc.gcm.NonceSize() {
		t.Fatalf("ciphertext len %d <= nonce size %d", len(enc), svc.gcm.NonceSize())
	}
	dec, err := svc.decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(dec, raw) {
		t.Fatalf("decrypt = %x, want %x", dec, raw)
	}
}

func TestMiddlewareValidKey(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	key, scope, err := svc.Bootstrap(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var seen domain.Scope
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		seen = ScopeFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "ApiKey "+key)
	svc.Middleware(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler not called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != scope {
		t.Fatalf("ScopeFrom = %+v, want %+v", seen, scope)
	}
}

func TestMiddlewareRejects(t *testing.T) {
	svc := newService(t)
	if _, _, err := svc.Bootstrap(context.Background(), "Acme", "production"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	cases := map[string]string{
		"missing":      "",
		"wrong-scheme": "Bearer abc",
		"empty-key":    "ApiKey ",
		"bad-hex":      "ApiKey zzzz",
		"unknown-key":  "ApiKey " + hex.EncodeToString(make([]byte, keyLen)),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			svc.Middleware(next).ServeHTTP(rec, req)
			if called {
				t.Fatal("next handler called on rejected request")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestScopeFromPanicsWithoutMiddleware(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("ScopeFrom without scope: expected panic")
		}
	}()
	ScopeFrom(context.Background())
}

// ─── DecryptForDisplay ────────────────────────────────────────────────────────

func TestDecryptForDisplay_MaskedAndRaw(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	rawHex, scope, err := svc.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	masked, last4, got, err := svc.DecryptForDisplay(ctx, scope)
	if err != nil {
		t.Fatalf("DecryptForDisplay: %v", err)
	}

	// raw key must round-trip through decrypt.
	if got != rawHex {
		t.Errorf("DecryptForDisplay rawHex = %q, want %q", got, rawHex)
	}
	// last4 must be the final 4 chars of the raw hex.
	if last4 != rawHex[len(rawHex)-4:] {
		t.Errorf("last4 = %q, want %q", last4, rawHex[len(rawHex)-4:])
	}
	// masked must end with last4 and NOT equal the full key.
	if !hasLast4Suffix(masked, last4) {
		t.Errorf("masked %q does not end with last4 %q", masked, last4)
	}
	if masked == got {
		t.Error("masked key equals raw key — masking failed")
	}
	// The full raw key must not appear anywhere in the masked output.
	if masked == rawHex {
		t.Error("masked key is identical to raw hex")
	}
}

func TestDecryptForDisplay_EmptyScope_Errors(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	_, _, _, err := svc.DecryptForDisplay(ctx, domain.Scope{OrgID: "no-org", EnvID: "no-env"})
	if err == nil {
		t.Error("DecryptForDisplay on empty scope: expected error, got nil")
	}
}

// hasLast4Suffix checks whether s ends with suffix.
func hasLast4Suffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

// ─── RotateKey ────────────────────────────────────────────────────────────────

func TestRotateKey_MintsNewKey(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	oldRaw, scope, err := svc.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Old key authenticates before rotation.
	if _, err := svc.Authenticate(ctx, oldRaw); err != nil {
		t.Fatalf("Authenticate before rotate: %v", err)
	}

	newRaw, err := svc.RotateKey(ctx, scope)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if newRaw == oldRaw {
		t.Error("RotateKey returned the same key as before")
	}

	// Old key no longer authenticates.
	if _, err := svc.Authenticate(ctx, oldRaw); err == nil {
		t.Error("old key still authenticates after rotation — revoke broken")
	}

	// New key authenticates to the same scope.
	got, err := svc.Authenticate(ctx, newRaw)
	if err != nil {
		t.Fatalf("Authenticate with new key: %v", err)
	}
	if got != scope {
		t.Errorf("Authenticate scope = %+v, want %+v", got, scope)
	}
}

func TestRotateKey_DoesNotAffectOtherScopes(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	key1, scope1, err := svc.Bootstrap(ctx, "OrgA", "dev")
	if err != nil {
		t.Fatalf("Bootstrap A: %v", err)
	}
	key2, _, err := svc.Bootstrap(ctx, "OrgB", "dev")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}

	if _, err := svc.RotateKey(ctx, scope1); err != nil {
		t.Fatalf("RotateKey scope1: %v", err)
	}

	// scope2's key is untouched.
	if _, err := svc.Authenticate(ctx, key2); err != nil {
		t.Errorf("Authenticate scope2 key after scope1 rotate: %v", err)
	}
	_ = key1 // only used to confirm scope1 rotation happened
}

// ─── CreateEnvironment ────────────────────────────────────────────────────────

func TestCreateEnvironment_MintsEnvAndKey(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	// Bootstrap creates the org.
	_, scope0, err := svc.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	rawHex, env, scope, err := svc.CreateEnvironment(ctx, scope0.OrgID, "Staging", "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.ID == "" {
		t.Error("CreateEnvironment: env.ID is empty")
	}
	if scope.OrgID != scope0.OrgID {
		t.Errorf("CreateEnvironment: scope.OrgID = %q, want %q", scope.OrgID, scope0.OrgID)
	}
	if scope.EnvID == scope0.EnvID {
		t.Error("CreateEnvironment: new env ID collides with bootstrap env ID")
	}

	// The minted key authenticates to the new scope.
	got, err := svc.Authenticate(ctx, rawHex)
	if err != nil {
		t.Fatalf("Authenticate new env key: %v", err)
	}
	if got != scope {
		t.Errorf("Authenticate scope = %+v, want %+v", got, scope)
	}
}
