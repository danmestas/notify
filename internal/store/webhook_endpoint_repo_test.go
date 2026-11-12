package store

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0004_AppliesWebhookEndpoints(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 55 {
		t.Fatalf("expected 55 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='webhook_endpoints'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("webhook_endpoints table missing: %v", err)
	}
}

func TestWebhookEndpointRepo_CreateListActive(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.WebhookEndpoints()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	active, err := repo.Create(ctx, scope, domain.WebhookEndpoint{
		URL:    "https://example.com/active",
		Active: true,
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	if active.ID == "" {
		t.Fatalf("expected minted id, got empty")
	}

	if _, err := repo.Create(ctx, scope, domain.WebhookEndpoint{
		URL:    "https://example.com/inactive",
		Active: false,
	}); err != nil {
		t.Fatalf("create inactive: %v", err)
	}

	got, err := repo.ListActiveByEnv(ctx, scope)
	if err != nil {
		t.Fatalf("ListActiveByEnv: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 active endpoint, got %d: %+v", len(got), got)
	}
	if got[0].ID != active.ID {
		t.Fatalf("expected active id %q, got %q", active.ID, got[0].ID)
	}
	if got[0].URL != "https://example.com/active" || !got[0].Active {
		t.Fatalf("unexpected endpoint: %+v", got[0])
	}

	// Cross-env scope isolation: an active endpoint in another env is not
	// returned for scope e1.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if _, err := repo.Create(ctx, other, domain.WebhookEndpoint{
		URL:    "https://example.com/other-env",
		Active: true,
	}); err != nil {
		t.Fatalf("create other-env: %v", err)
	}

	got, err = repo.ListActiveByEnv(ctx, scope)
	if err != nil {
		t.Fatalf("ListActiveByEnv after cross-env insert: %v", err)
	}
	if len(got) != 1 || got[0].ID != active.ID {
		t.Fatalf("scope isolation broken: got %+v", got)
	}
}

func TestWebhookEndpointRepo_List_IncludesInactive(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.WebhookEndpoints()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Create(ctx, scope, domain.WebhookEndpoint{URL: "https://a.com", Active: true}); err != nil {
		t.Fatalf("create active: %v", err)
	}
	if _, err := repo.Create(ctx, scope, domain.WebhookEndpoint{URL: "https://b.com", Active: false}); err != nil {
		t.Fatalf("create inactive: %v", err)
	}

	all, err := repo.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List = %d endpoints, want 2", len(all))
	}
}

func TestWebhookEndpointRepo_GetByID_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.WebhookEndpoints()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	ep, err := repo.Create(ctx, scope, domain.WebhookEndpoint{URL: "https://a.com", Active: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, scope, ep.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.URL != "https://a.com" {
		t.Errorf("URL = %q, want https://a.com", got.URL)
	}

	if _, err := repo.GetByID(ctx, other, ep.ID); err == nil {
		t.Errorf("cross-scope GetByID should fail, got nil")
	}
}

func TestWebhookEndpointRepo_UpdateURL(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.WebhookEndpoints()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	ep, err := repo.Create(ctx, scope, domain.WebhookEndpoint{URL: "https://old.com", Active: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := repo.UpdateURL(ctx, scope, ep.ID, "https://new.com")
	if err != nil {
		t.Fatalf("UpdateURL: %v", err)
	}
	if updated.URL != "https://new.com" {
		t.Errorf("URL = %q, want https://new.com", updated.URL)
	}

	if _, err := repo.UpdateURL(ctx, scope, "nonexistent", "https://x.com"); err == nil {
		t.Errorf("UpdateURL nonexistent should fail")
	}
}

func TestWebhookEndpointRepo_SetActive(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.WebhookEndpoints()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	ep, err := repo.Create(ctx, scope, domain.WebhookEndpoint{URL: "https://a.com", Active: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	disabled, err := repo.SetActive(ctx, scope, ep.ID, false)
	if err != nil {
		t.Fatalf("SetActive false: %v", err)
	}
	if disabled.Active {
		t.Errorf("expected Active=false after disable")
	}

	active, err := repo.SetActive(ctx, scope, ep.ID, true)
	if err != nil {
		t.Fatalf("SetActive true: %v", err)
	}
	if !active.Active {
		t.Errorf("expected Active=true after re-enable")
	}
}

func TestWebhookEndpointRepo_Delete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.WebhookEndpoints()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	ep, err := repo.Create(ctx, scope, domain.WebhookEndpoint{URL: "https://a.com", Active: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repo.Delete(ctx, scope, ep.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, scope, ep.ID); err == nil {
		t.Errorf("GetByID after delete should fail")
	}

	// Second delete is ErrNotFound.
	if err := repo.Delete(ctx, scope, ep.ID); err == nil {
		t.Errorf("second Delete should fail")
	}
}

// TestMigrate_0055_DedupesKeepingOldestActive: duplicates written before the
// unique index are collapsed to the oldest active row per (env, url); other
// environments and distinct URLs are untouched.
func TestMigrate_0055_DedupesKeepingOldestActive(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.writer.ExecContext(ctx, `DROP INDEX webhook_endpoint_env_url`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	for _, r := range []struct {
		id, env, url string
		active       int
	}{
		{"01", "e1", "https://a.example/h", 0}, // oldest but inactive
		{"02", "e1", "https://a.example/h", 1}, // oldest active: kept
		{"03", "e1", " https://a.example/h", 1},
		{"04", "e2", "https://a.example/h", 0}, // other env: kept
		{"05", "e3", "https://b.example/h", 0}, // only inactive: oldest kept
		{"06", "e3", "https://b.example/h", 0},
		{"07", "e1", "https://c.example/h", 1}, // distinct url: kept
	} {
		if _, err := s.writer.ExecContext(ctx,
			`INSERT INTO webhook_endpoints (id, org_id, environment_id, url, active) VALUES (?, 'o', ?, ?, ?)`,
			r.id, r.env, r.url, r.active); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	body, err := migrationsFS.ReadFile("migrations/0055_webhook_endpoint_unique_url.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 0055: %v", err)
	}
	rows, err := s.writer.QueryContext(ctx, `SELECT id FROM webhook_endpoints ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if want := "02 04 05 07"; strings.Join(got, " ") != want {
		t.Fatalf("kept %v, want %s", got, want)
	}
}
