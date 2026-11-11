package notify_test

// Methodology: EnsureTenant runs against a real in-process dagnats and SQLite.
// The returned key is proven by one authenticated request through the
// handler, and the program path by running notify under program.Run.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/dagnats/program"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/notify"
)

func subscribersStatus(t *testing.T, h http.Handler, apiKey string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/subscribers", nil)
	req.Header.Set("Authorization", "ApiKey "+apiKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestNotify_EnsureTenant(t *testing.T) {
	n, _ := newNotify(t, "")
	ctx := context.Background()
	spec := notify.TenantSpec{Organization: "Acme", Environment: "production"}

	first, err := n.EnsureTenant(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if !first.Created || first.APIKey == "" || first.OrgID == "" || first.EnvID == "" {
		t.Fatalf("first = %+v, want Created with a key and ids", first)
	}
	if got := subscribersStatus(t, n.Handler(), first.APIKey); got != http.StatusOK {
		t.Fatalf("authenticated request with the minted key = %d, want 200", got)
	}
	if got := subscribersStatus(t, n.Handler(), "not-a-key"); got != http.StatusUnauthorized {
		t.Fatalf("request with a bad key = %d, want 401", got)
	}

	again, err := n.EnsureTenant(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureTenant again: %v", err)
	}
	if again.Created || again.APIKey != "" || again.OrgID != first.OrgID || again.EnvID != first.EnvID {
		t.Fatalf("again = %+v, want the same ids, no key, Created=false", again)
	}
}

func TestNotify_EnsureTenantRejectsEmptySpec(t *testing.T) {
	n, _ := newNotify(t, "")
	_, err := n.EnsureTenant(context.Background(), notify.TenantSpec{Organization: "Acme"})
	if !errors.Is(err, notify.ErrTenantSpec) {
		t.Fatalf("EnsureTenant without an environment = %v, want ErrTenantSpec", err)
	}
}

func TestProgram_EnsureTenant(t *testing.T) {
	ctx := context.Background()
	spec := notify.TenantSpec{Organization: "Acme", Environment: "production"}

	p := &notify.Program{Config: notify.Config{
		DBPath:    filepath.Join(t.TempDir(), "notify.db"),
		MasterKey: make([]byte, 32),
	}}
	if _, err := p.EnsureTenant(ctx, spec); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("EnsureTenant before Start = %v, want a not-running error", err)
	}

	opts := program.Options{URL: dagtest.StartSidecar(t), SeedPath: filepath.Join(t.TempDir(), "notify.seed")}
	running, err := program.Run(ctx, p, opts)
	if err != nil {
		t.Fatalf("program.Run: %v", err)
	}
	t.Cleanup(func() { _ = running.Stop(context.Background()) })

	tenant, err := p.EnsureTenant(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureTenant after Start: %v", err)
	}
	if !tenant.Created || tenant.APIKey == "" {
		t.Fatalf("tenant = %+v, want Created with a key", tenant)
	}
	if got := subscribersStatus(t, p.Handler(), tenant.APIKey); got != http.StatusOK {
		t.Fatalf("request through Program.Handler = %d, want 200", got)
	}
}
