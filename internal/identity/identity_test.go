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
