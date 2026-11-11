package identity

// Methodology: EnsureTenant is exercised against a real SQLite store. The
// minted key is proven by authenticating with it, never by inspecting storage.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func TestEnsureTenantCreatesThenFinds(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	first, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if !first.KeyIssued || first.APIKey == "" || first.Scope.OrgID == "" || first.Scope.EnvID == "" {
		t.Fatalf("first call = %+v, want KeyIssued with a key and scope", first)
	}
	got, err := svc.Authenticate(ctx, first.APIKey)
	if err != nil || got != first.Scope {
		t.Fatalf("Authenticate = %+v, %v; want %+v", got, err, first.Scope)
	}

	again, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant again: %v", err)
	}
	if again.KeyIssued || again.APIKey != "" || again.Scope != first.Scope {
		t.Fatalf("second call = %+v, want the same scope, no key, KeyIssued=false", again)
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
	if !dev.KeyIssued || dev.APIKey == "" {
		t.Fatalf("dev = %+v, want KeyIssued with a key", dev)
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

func TestEnsureTenantRepeatKeepsFirstKeyValid(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	first, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if _, err := svc.EnsureTenant(ctx, "Acme", "production"); err != nil {
		t.Fatalf("EnsureTenant again: %v", err)
	}
	got, err := svc.Authenticate(ctx, first.APIKey)
	if err != nil || got != first.Scope {
		t.Fatalf("first key after repeat = %+v, %v; want %+v", got, err, first.Scope)
	}
}

func TestEnsureTenantSecondEnvKeepsFirstKeyValid(t *testing.T) {
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
	if dev.APIKey == prod.APIKey {
		t.Fatal("second environment reused the first key")
	}
	for _, c := range []Tenant{prod, dev} {
		got, err := svc.Authenticate(ctx, c.APIKey)
		if err != nil || got != c.Scope {
			t.Fatalf("Authenticate = %+v, %v; want %+v", got, err, c.Scope)
		}
	}
}

func TestEnsureTenantIssuesKeyForKeylessEnv(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	// An older crash left an organization and environment with no key.
	org, err := svc.store.Organizations().Create(ctx, "Acme")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	_, scope, err := svc.store.Environments().Create(ctx, org.ID, domain.Environment{
		Name: "production", Identifier: "production",
	})
	if err != nil {
		t.Fatalf("create env: %v", err)
	}

	got, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if !got.KeyIssued || got.APIKey == "" || got.Scope != scope {
		t.Fatalf("got %+v, want a key issued for %+v", got, scope)
	}
	if s, err := svc.Authenticate(ctx, got.APIKey); err != nil || s != scope {
		t.Fatalf("Authenticate = %+v, %v; want %+v", s, err, scope)
	}
	again, err := svc.EnsureTenant(ctx, "Acme", "production")
	if err != nil || again.KeyIssued || again.APIKey != "" {
		t.Fatalf("again = %+v, %v; want no second key", again, err)
	}
}

func TestEnsureTenantNormalisesInput(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	first, err := svc.EnsureTenant(ctx, "  Acme ", " Staging East ")
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	env, err := svc.store.Environments().Get(ctx, first.Scope)
	if err != nil {
		t.Fatalf("Get env: %v", err)
	}
	if env.Identifier != "staging-east" || env.Name != "Staging East" {
		t.Fatalf("env = %+v, want identifier staging-east and name Staging East", env)
	}
	again, err := svc.EnsureTenant(ctx, "Acme", "staging-east")
	if err != nil || again.Scope != first.Scope || again.KeyIssued {
		t.Fatalf("again = %+v, %v; want the same env found", again, err)
	}
	for _, c := range [][2]string{{"   ", "dev"}, {"Acme", "  \t"}} {
		if _, err := svc.EnsureTenant(ctx, c[0], c[1]); !errors.Is(err, ErrTenantSpec) {
			t.Errorf("EnsureTenant(%q, %q) = %v, want ErrTenantSpec", c[0], c[1], err)
		}
	}
}

// Two Services over two store.Opens of one file stand in for two processes
// (the CLI plus a host): only the database can serialise them.
func TestEnsureTenantConcurrentAcrossStores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	key := testKey(t)
	var svcs []*Service
	for i := 0; i < 2; i++ {
		s, err := store.Open(ctx, path)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		svcs = append(svcs, New(s, key))
	}

	const perService = 25
	results := make(chan Tenant, 2*perService)
	errs := make(chan error, 2*perService)
	var wg sync.WaitGroup
	for _, svc := range svcs {
		for i := 0; i < perService; i++ {
			wg.Add(1)
			go func(svc *Service) {
				defer wg.Done()
				tn, err := svc.EnsureTenant(ctx, "Acme", "production")
				if err != nil {
					errs <- err
					return
				}
				results <- tn
			}(svc)
		}
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("EnsureTenant: %v", err)
	}
	issued := 0
	var want domain.Scope
	for tn := range results {
		if want == (domain.Scope{}) {
			want = tn.Scope
		}
		if tn.Scope != want {
			t.Fatalf("scope %+v differs from %+v", tn.Scope, want)
		}
		if tn.KeyIssued {
			issued++
		}
	}
	if issued != 1 {
		t.Fatalf("%d keys issued, want exactly 1", issued)
	}
	orgs, err := svcs[0].store.Organizations().FindByName(ctx, "Acme")
	if err != nil || len(orgs) != 1 {
		t.Fatalf("orgs = %d, %v; want 1", len(orgs), err)
	}
	envs, err := svcs[0].store.Environments().List(ctx, orgs[0].ID)
	if err != nil || len(envs) != 1 {
		t.Fatalf("envs = %d, %v; want 1", len(envs), err)
	}
	keys, err := svcs[0].store.ApiKeys().List(ctx, want)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %d, %v; want 1", len(keys), err)
	}
}
