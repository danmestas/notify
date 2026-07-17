package inbounddomains

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestNew_MXHostParam pins the constructor contract after lifting the
// NOTIFY_INBOUND_MX_HOST env read up to cmd/notify: an injected host is used
// verbatim, and an empty host falls back to defaultMXHost.
func TestNew_MXHostParam(t *testing.T) {
	st := openTestStore(t)

	if got := New(st, "mx.injected.test").MXHost(); got != "mx.injected.test" {
		t.Errorf("MXHost with injected host = %q, want mx.injected.test", got)
	}
	if got := New(st, "").MXHost(); got != defaultMXHost {
		t.Errorf("MXHost with empty host = %q, want %q (default fallback)", got, defaultMXHost)
	}
}

func TestService_Create(t *testing.T) {
	st := openTestStore(t)
	svc := newWithLookup(st, "mx.notify.test", net.LookupMX)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	ctx := context.Background()

	d, err := svc.Create(ctx, scope, "test.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if d.Status != "pending" {
		t.Errorf("Status = %q, want pending", d.Status)
	}
	if d.ExpectedMXHost != "mx.notify.test" {
		t.Errorf("ExpectedMXHost = %q, want mx.notify.test", d.ExpectedMXHost)
	}
}

func TestService_Verify_MXFound(t *testing.T) {
	st := openTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	ctx := context.Background()

	// Stub that returns the expected MX.
	mockLookup := func(host string) ([]*net.MX, error) {
		return []*net.MX{{Host: "mx.notify.test.", Pref: 10}}, nil
	}
	svc := newWithLookup(st, "mx.notify.test", mockLookup)

	if _, err := svc.Create(ctx, scope, "verified.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Verify(ctx, scope, "verified.com")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Status != "verified" {
		t.Errorf("Status = %q, want verified", got.Status)
	}
	if !got.MXConfigured {
		t.Error("MXConfigured should be true")
	}
}

func TestService_Verify_MXNotFound(t *testing.T) {
	st := openTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	ctx := context.Background()

	// Stub that returns a different MX host.
	mockLookup := func(host string) ([]*net.MX, error) {
		return []*net.MX{{Host: "other-mx.com.", Pref: 10}}, nil
	}
	svc := newWithLookup(st, "mx.notify.test", mockLookup)

	if _, err := svc.Create(ctx, scope, "unverified.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Verify(ctx, scope, "unverified.com")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.MXConfigured {
		t.Error("MXConfigured should be false")
	}
}

func TestService_Verify_DNSError(t *testing.T) {
	st := openTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	ctx := context.Background()

	mockLookup := func(host string) ([]*net.MX, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	svc := newWithLookup(st, "mx.notify.test", mockLookup)

	if _, err := svc.Create(ctx, scope, "dns-fail.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := svc.Verify(ctx, scope, "dns-fail.com")
	if err != nil {
		t.Fatalf("Verify should not error on DNS failure: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
}

func TestService_Delete(t *testing.T) {
	st := openTestStore(t)
	svc := newWithLookup(st, "mx.notify.test", net.LookupMX)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	ctx := context.Background()

	if _, err := svc.Create(ctx, scope, "del.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, scope, "del.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, scope, "del.com"); err != store.ErrNotFound {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}
