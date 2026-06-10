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
