package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestWorkflowRepo_GetByTriggerIdentifier_Scoped(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Workflows()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	created, err := repo.Create(ctx, scopeA, domain.Workflow{
		Name: "Welcome", TriggerIdentifier: "welcome", Active: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByTriggerIdentifier(ctx, scopeA, "welcome")
	if err != nil {
		t.Fatalf("get within scope: %v", err)
	}
	if got.ID != created.ID || got.Name != "Welcome" || !got.Active {
		t.Fatalf("unexpected workflow: %+v", got)
	}

	// Cross-env fetch of the same trigger identifier must not find it.
	if _, err := repo.GetByTriggerIdentifier(ctx, scopeB, "welcome"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound cross-env, got %v", err)
	}
}

func TestWorkflowRepo_GetByID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Workflows()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	created, err := repo.Create(ctx, scopeA, domain.Workflow{
		Name: "Welcome", TriggerIdentifier: "welcome", Active: true,
		Steps: []byte(`[{"id":"s1","type":"email"}]`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, scopeA, created.ID)
	if err != nil {
		t.Fatalf("GetByID within scope: %v", err)
	}
	if got.ID != created.ID || got.Name != "Welcome" || !got.Active {
		t.Fatalf("unexpected workflow: %+v", got)
	}
	if string(got.Steps) != `[{"id":"s1","type":"email"}]` {
		t.Fatalf("steps mismatch: %s", got.Steps)
	}

	// Cross-env scope must not find it.
	if _, err := repo.GetByID(ctx, scopeB, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound cross-env, got %v", err)
	}

	// Unknown id must not be found.
	if _, err := repo.GetByID(ctx, scopeA, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown id, got %v", err)
	}
}

func TestWorkflowRepo_Update(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Workflows()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	created, err := repo.Create(ctx, scopeA, domain.Workflow{
		Name: "Original", TriggerIdentifier: "orig", Active: true,
		Steps: []byte(`[{"stepId":"s1","type":"in_app"}]`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update the name and steps.
	updated := created
	updated.Name = "Updated"
	updated.Steps = []byte(`[{"stepId":"s2","type":"email"}]`)
	if err := repo.Update(ctx, scopeA, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, scopeA, created.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Name != "Updated" {
		t.Fatalf("name = %q, want Updated", got.Name)
	}
	if string(got.Steps) != `[{"stepId":"s2","type":"email"}]` {
		t.Fatalf("steps = %s, want updated steps", got.Steps)
	}
	// TriggerIdentifier must be immutable.
	if got.TriggerIdentifier != "orig" {
		t.Fatalf("trigger identifier must not change, got %q", got.TriggerIdentifier)
	}

	// Update with wrong scope must return ErrNotFound.
	if err := repo.Update(ctx, scopeB, updated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-scope Update, got %v", err)
	}

	// Update with unknown ID must return ErrNotFound.
	ghost := updated
	ghost.ID = "nonexistent"
	if err := repo.Update(ctx, scopeA, ghost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown ID, got %v", err)
	}
}

func TestWorkflowRepo_Delete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Workflows()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	_, err := repo.Create(ctx, scopeA, domain.Workflow{
		Name: "Bye", TriggerIdentifier: "bye", Active: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Cross-scope delete must not affect scopeA's row.
	if err := repo.Delete(ctx, scopeB, "bye"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-scope Delete, got %v", err)
	}

	// Row must still exist in scopeA.
	if _, err := repo.GetByTriggerIdentifier(ctx, scopeA, "bye"); err != nil {
		t.Fatalf("GetByTriggerIdentifier after cross-scope delete failed: %v", err)
	}

	// Delete within scope must succeed.
	if err := repo.Delete(ctx, scopeA, "bye"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Row must be gone.
	if _, err := repo.GetByTriggerIdentifier(ctx, scopeA, "bye"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}

	// Deleting again must return ErrNotFound.
	if err := repo.Delete(ctx, scopeA, "bye"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for double-delete, got %v", err)
	}
}

func TestWorkflowRepo_SetActive(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Workflows()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	_, err := repo.Create(ctx, scopeA, domain.Workflow{
		Name: "Toggle", TriggerIdentifier: "toggle", Active: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Deactivate.
	if err := repo.SetActive(ctx, scopeA, "toggle", false); err != nil {
		t.Fatalf("SetActive false: %v", err)
	}
	got, err := repo.GetByTriggerIdentifier(ctx, scopeA, "toggle")
	if err != nil {
		t.Fatalf("GetByTriggerIdentifier: %v", err)
	}
	if got.Active {
		t.Fatalf("expected active=false after deactivation")
	}

	// Re-activate.
	if err := repo.SetActive(ctx, scopeA, "toggle", true); err != nil {
		t.Fatalf("SetActive true: %v", err)
	}
	got, err = repo.GetByTriggerIdentifier(ctx, scopeA, "toggle")
	if err != nil {
		t.Fatalf("GetByTriggerIdentifier after re-activate: %v", err)
	}
	if !got.Active {
		t.Fatalf("expected active=true after re-activation")
	}

	// Cross-scope SetActive must return ErrNotFound.
	if err := repo.SetActive(ctx, scopeB, "toggle", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-scope SetActive, got %v", err)
	}

	// Unknown identifier must return ErrNotFound.
	if err := repo.SetActive(ctx, scopeA, "missing", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown identifier, got %v", err)
	}
}
