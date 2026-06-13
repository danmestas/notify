package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestEnvironmentRepo_ListUpdateDeleteCountByOrg(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Bootstrap: create an org first (using the org repo directly).
	org, err := s.Organizations().Create(ctx, "TestOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// CountByOrg on fresh org returns 0.
	n, err := s.Environments().CountByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("CountByOrg empty: %v", err)
	}
	if n != 0 {
		t.Errorf("CountByOrg empty: got %d, want 0", n)
	}

	// Create two environments.
	env1, _, err := s.Environments().Create(ctx, org.ID, domain.Environment{
		Name: "dev", Identifier: "dev",
	})
	if err != nil {
		t.Fatalf("create env1: %v", err)
	}
	env2, _, err := s.Environments().Create(ctx, org.ID, domain.Environment{
		Name: "prod", Identifier: "prod",
	})
	if err != nil {
		t.Fatalf("create env2: %v", err)
	}

	// List returns both.
	envs, err := s.Environments().List(ctx, org.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("List: got %d envs, want 2", len(envs))
	}
	if envs[0].ID != env1.ID || envs[1].ID != env2.ID {
		t.Errorf("List order wrong: got %v %v", envs[0].ID, envs[1].ID)
	}

	// CountByOrg returns 2.
	n, err = s.Environments().CountByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("CountByOrg: %v", err)
	}
	if n != 2 {
		t.Errorf("CountByOrg: got %d, want 2", n)
	}

	// Update renames env1.
	if err := s.Environments().Update(ctx, org.ID, env1.ID, "development"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	envs, err = s.Environments().List(ctx, org.ID)
	if err != nil {
		t.Fatalf("List after update: %v", err)
	}
	if envs[0].Name != "development" {
		t.Errorf("Update: name = %q, want %q", envs[0].Name, "development")
	}

	// Update on unknown ID returns ErrNotFound.
	if err := s.Environments().Update(ctx, org.ID, "nonexistent", "x"); err == nil {
		t.Error("Update on unknown ID: want ErrNotFound, got nil")
	}

	// Delete removes env2.
	if err := s.Environments().Delete(ctx, org.ID, env2.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	n, err = s.Environments().CountByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("CountByOrg after delete: %v", err)
	}
	if n != 1 {
		t.Errorf("CountByOrg after delete: got %d, want 1", n)
	}

	// Delete on unknown ID returns ErrNotFound.
	if err := s.Environments().Delete(ctx, org.ID, env2.ID); err == nil {
		t.Error("Delete on already-deleted ID: want ErrNotFound, got nil")
	}

	// List isolation: a second org sees no envs from the first.
	org2, err := s.Organizations().Create(ctx, "OtherOrg")
	if err != nil {
		t.Fatalf("create org2: %v", err)
	}
	envs2, err := s.Environments().List(ctx, org2.ID)
	if err != nil {
		t.Fatalf("List org2: %v", err)
	}
	if len(envs2) != 0 {
		t.Errorf("List org2: got %d envs, want 0 (scope isolation broken)", len(envs2))
	}
}
