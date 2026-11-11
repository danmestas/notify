package identity

// Methodology: EnsureTenant is exercised against a real SQLite store. The
// minted key is proven by authenticating with it, never by inspecting storage.

import (
	"context"
	"errors"
	"testing"
)

func TestEnsureTenantCreatesThenFinds(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	first, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if !first.Created || first.APIKey == "" || first.Scope.OrgID == "" || first.Scope.EnvID == "" {
		t.Fatalf("first call = %+v, want Created with a key and scope", first)
	}
	got, err := svc.Authenticate(ctx, first.APIKey)
	if err != nil || got != first.Scope {
		t.Fatalf("Authenticate = %+v, %v; want %+v", got, err, first.Scope)
	}

	again, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant again: %v", err)
	}
	if again.Created || again.APIKey != "" || again.Scope != first.Scope {
		t.Fatalf("second call = %+v, want the same scope, no key, Created=false", again)
	}
}

func TestEnsureTenantNewEnvInExistingOrg(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	prod, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	dev, err := svc.EnsureTenant(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("EnsureTenant dev: %v", err)
	}
	if !dev.Created || dev.APIKey == "" {
		t.Fatalf("dev = %+v, want Created with a key", dev)
	}
	if dev.Scope.OrgID != prod.Scope.OrgID || dev.Scope.EnvID == prod.Scope.EnvID {
		t.Fatalf("dev = %+v, prod = %+v: want a new env under the same org", dev.Scope, prod.Scope)
	}
}

func TestEnsureTenantRejectsEmptyAndAmbiguous(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, c := range [][2]string{{"", "dev"}, {"Acme", ""}} {
		if _, err := svc.EnsureTenant(ctx, c[0], c[1]); !errors.Is(err, ErrTenantSpec) {
			t.Errorf("EnsureTenant(%q, %q) = %v, want ErrTenantSpec", c[0], c[1], err)
		}
	}
	// Bootstrap allows two orgs of one name; which the spec means is then
	// unknowable, so EnsureTenant refuses rather than guessing.
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Bootstrap(ctx, "Twin", "dev"); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
	}
	if _, err := svc.EnsureTenant(ctx, "Twin", "dev"); !errors.Is(err, ErrTenantConflict) {
		t.Fatalf("EnsureTenant on ambiguous org = %v, want ErrTenantConflict", err)
	}
}
