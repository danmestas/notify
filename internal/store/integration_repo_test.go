package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0002_AppliesIntegrations(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 7 {
		t.Fatalf("expected 7 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='integrations'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("integrations table missing: %v", err)
	}
}

func TestIntegrationRepo_GetActiveByChannel_PrefersPrimary(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Non-primary with lower priority value (would win on priority alone).
	if _, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true,
		Primary: false, Priority: 0, Identifier: "smtp-secondary",
	}); err != nil {
		t.Fatalf("create secondary: %v", err)
	}
	primary, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "capture", Channel: "email", Active: true,
		Primary: true, Priority: 5, Identifier: "capture-primary",
	})
	if err != nil {
		t.Fatalf("create primary: %v", err)
	}

	got, err := repo.GetActiveByChannel(ctx, scope, "email")
	if err != nil {
		t.Fatalf("GetActiveByChannel: %v", err)
	}
	if got.ID != primary.ID {
		t.Fatalf("expected primary %q, got %q", primary.ID, got.ID)
	}
	if !got.Primary {
		t.Fatalf("expected primary_provider=1, got %+v", got)
	}
}

func TestIntegrationRepo_GetActiveByChannel_PriorityThenScope(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	// No primary: lowest priority wins.
	if _, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true,
		Priority: 10, Identifier: "high-prio",
	}); err != nil {
		t.Fatalf("create high-prio: %v", err)
	}
	low, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "capture", Channel: "email", Active: true,
		Priority: 1, Identifier: "low-prio",
	})
	if err != nil {
		t.Fatalf("create low-prio: %v", err)
	}
	// Inactive must be excluded even with the lowest priority.
	if _, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: false,
		Priority: 0, Identifier: "inactive",
	}); err != nil {
		t.Fatalf("create inactive: %v", err)
	}

	got, err := repo.GetActiveByChannel(ctx, e1, "email")
	if err != nil {
		t.Fatalf("GetActiveByChannel e1: %v", err)
	}
	if got.ID != low.ID {
		t.Fatalf("expected lowest priority %q, got %q", low.ID, got.ID)
	}

	// Scope isolation: an integration under e2 only is invisible to e1; and e1
	// rows are invisible to e2.
	if _, err := repo.Create(ctx, e2, domain.Integration{
		ProviderID: "smtp", Channel: "sms", Active: true,
		Priority: 0, Identifier: "e2-only",
	}); err != nil {
		t.Fatalf("create e2-only: %v", err)
	}
	if _, err := repo.GetActiveByChannel(ctx, e1, "sms"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for sms under e1, got %v", err)
	}
	if _, err := repo.GetActiveByChannel(ctx, e2, "email"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for email under e2, got %v", err)
	}
}
