package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0004_AppliesWebhookEndpoints(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 9 {
		t.Fatalf("expected 9 recorded migrations, got %d", n)
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
