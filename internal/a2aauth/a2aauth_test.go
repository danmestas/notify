package a2aauth_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/a2aauth"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

type fixture struct {
	st     *store.Store
	id     *identity.Service
	svc    *a2aauth.Service
	log    *bytes.Buffer
	scopeA domain.Scope
	scopeB domain.Scope
	apiKey string
	handle http.Handler
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	apiKey, scopeA, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("bootstrap A: %v", err)
	}
	_, scopeB, err := id.Bootstrap(ctx, "Other", "prod")
	if err != nil {
		t.Fatalf("bootstrap B: %v", err)
	}
	if _, err := st.Subscribers().Upsert(ctx, scopeA, domain.Subscriber{SubscriberID: "alice"}); err != nil {
		t.Fatalf("upsert alice: %v", err)
	}
	buf := &bytes.Buffer{}
	svc := a2aauth.New(st, id, slog.New(slog.NewTextHandler(buf, nil)))
	// A test-only handler standing in for the /a2a/* routes (#75): it reports
	// the Principal the middleware resolved.
	h := svc.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a2aauth.FromContext(r.Context())
		if !ok {
			t.Error("no principal in context behind Authenticate")
		}
		_, _ = io.WriteString(w, strings.Join([]string{
			p.Scope.OrgID, p.Scope.EnvID, p.SubscriberID, p.CredentialID, string(p.Kind),
		}, "|"))
	}))
	return fixture{st: st, id: id, svc: svc, log: buf, scopeA: scopeA, scopeB: scopeB, apiKey: apiKey, handle: h}
}

func (f fixture) call(authz string) (int, string) {
	r := httptest.NewRequest(http.MethodPost, "/a2a/agent", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	f.handle.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func TestClientKey_MintAuthenticatesInOwnEnv(t *testing.T) {
	f := newFixture(t)
	key, tok, err := f.svc.Mint(context.Background(), f.scopeA, "billing", "")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(tok, a2aauth.TokenPrefix) {
		t.Fatalf("token %q lacks prefix", tok)
	}
	code, body := f.call("Bearer " + tok)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	want := strings.Join([]string{f.scopeA.OrgID, f.scopeA.EnvID, "", key.ID, "client_key"}, "|")
	if body != want {
		t.Errorf("principal = %q, want %q", body, want)
	}
	// The principal never names the other environment: a handler resolving
	// agents inside p.Scope cannot reach env B's agents.
	if strings.Contains(body, f.scopeB.EnvID) || strings.Contains(body, f.scopeB.OrgID) {
		t.Errorf("principal leaks other scope: %q", body)
	}
}

func TestClientKey_BoundSubscriber(t *testing.T) {
	f := newFixture(t)
	_, tok, err := f.svc.Mint(context.Background(), f.scopeA, "as-alice", "alice")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, body := f.call("Bearer " + tok)
	if parts := strings.Split(body, "|"); parts[2] != "alice" {
		t.Errorf("subscriber = %q, want alice", parts[2])
	}
	if _, _, err := f.svc.Mint(context.Background(), f.scopeA, "ghost", "nobody"); !errors.Is(err, a2aauth.ErrSubscriberNotFound) {
		t.Errorf("unknown subscriber: err = %v, want ErrSubscriberNotFound", err)
	}
	// alice exists in env A only.
	if _, _, err := f.svc.Mint(context.Background(), f.scopeB, "cross", "alice"); !errors.Is(err, a2aauth.ErrSubscriberNotFound) {
		t.Errorf("cross-env subscriber: err = %v, want ErrSubscriberNotFound", err)
	}
}

func TestClientKey_RevokedIs401(t *testing.T) {
	f := newFixture(t)
	key, tok, _ := f.svc.Mint(context.Background(), f.scopeA, "billing", "")
	if code, _ := f.call("Bearer " + tok); code != http.StatusOK {
		t.Fatalf("pre-revoke status = %d", code)
	}
	// Another env cannot revoke it.
	if err := f.svc.Revoke(context.Background(), f.scopeB, key.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-env revoke: err = %v, want ErrNotFound", err)
	}
	if err := f.svc.Revoke(context.Background(), f.scopeA, key.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if code, _ := f.call("Bearer " + tok); code != http.StatusUnauthorized {
		t.Errorf("revoked status = %d, want 401", code)
	}
	if err := f.svc.Revoke(context.Background(), f.scopeA, key.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second revoke: err = %v, want ErrNotFound", err)
	}
}

func TestAuthenticate_RejectsTenantAPIKey(t *testing.T) {
	f := newFixture(t)
	for _, authz := range []string{
		"ApiKey " + f.apiKey,
		"Bearer " + f.apiKey,
		"",
		"Bearer ",
		"Bearer " + a2aauth.TokenPrefix + "zz",
		"Bearer " + a2aauth.TokenPrefix + strings.Repeat("00", 32),
	} {
		if code, _ := f.call(authz); code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status = %d, want 401", authz, code)
		}
	}
}

func TestAuthenticate_AcceptsSessionToken(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.id.MintSubscriberSession(f.scopeA, "alice")
	code, body := f.call("Bearer " + tok)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	want := strings.Join([]string{f.scopeA.OrgID, f.scopeA.EnvID, "alice", "session:alice", "session"}, "|")
	if body != want {
		t.Errorf("principal = %q, want %q", body, want)
	}
	if code, _ := f.call("Bearer " + tok + "x"); code != http.StatusUnauthorized {
		t.Errorf("tampered session status = %d, want 401", code)
	}
}

func TestClientKey_SecretNotStoredInPlaintext(t *testing.T) {
	f := newFixture(t)
	_, tok, _ := f.svc.Mint(context.Background(), f.scopeA, "billing", "")
	secret := strings.TrimPrefix(tok, a2aauth.TokenPrefix)
	writer, _ := f.st.Pools()
	rows, err := writer.Query(`SELECT id, org_id, environment_id, name, subscriber_id, secret_hash, created_at, last_used_at, revoked_at FROM a2a_client_keys`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
		cols := make([]string, 9)
		ptrs := make([]any, 9)
		for i := range cols {
			ptrs[i] = &cols[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, c := range cols {
			if strings.Contains(c, secret) || strings.Contains(c, tok) {
				t.Errorf("column holds the plaintext secret: %q", c)
			}
		}
		if len(cols[5]) != 64 {
			t.Errorf("secret_hash = %q, want 64-char SHA-256 hex", cols[5])
		}
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestAudit_RecordsMintUseRevokeNeverTheToken(t *testing.T) {
	f := newFixture(t)
	key, tok, _ := f.svc.Mint(context.Background(), f.scopeA, "billing", "")
	f.call("Bearer " + tok)
	if err := f.svc.Revoke(context.Background(), f.scopeA, key.ID); err != nil {
		t.Fatal(err)
	}
	out := f.log.String()
	for _, ev := range []string{"a2a.client_key.mint", "a2a.credential.use", "a2a.client_key.revoke"} {
		if !strings.Contains(out, ev) {
			t.Errorf("audit log missing %s:\n%s", ev, out)
		}
	}
	if !strings.Contains(out, key.ID) {
		t.Errorf("audit log missing credential id")
	}
	if strings.Contains(out, strings.TrimPrefix(tok, a2aauth.TokenPrefix)) {
		t.Errorf("audit log contains the token:\n%s", out)
	}
}

func TestList_HidesHashAndTracksUse(t *testing.T) {
	f := newFixture(t)
	_, tok, _ := f.svc.Mint(context.Background(), f.scopeA, "billing", "")
	f.call("Bearer " + tok)
	keys, err := f.svc.List(context.Background(), f.scopeA)
	if err != nil || len(keys) != 1 {
		t.Fatalf("List = %v, %v", keys, err)
	}
	if keys[0].SecretHash != "" {
		t.Errorf("List leaks secret hash")
	}
	if keys[0].LastUsedAt == "" {
		t.Errorf("last_used_at not recorded")
	}
	if other, _ := f.svc.List(context.Background(), f.scopeB); len(other) != 0 {
		t.Errorf("env B sees env A's keys: %v", other)
	}
}

func TestMint_ValidatesName(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", 65)} {
		if _, _, err := f.svc.Mint(context.Background(), f.scopeA, name, ""); !errors.Is(err, a2aauth.ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
}
