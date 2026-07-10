package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0012_AppliesContexts(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 29 {
		t.Fatalf("expected 27 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "contexts",
	).Scan(&name)
	if err != nil {
		t.Fatalf("contexts table missing: %v", err)
	}
}

func TestContextRepo_CRUD(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.Contexts()

	data := json.RawMessage(`{"plan":"enterprise"}`)
	c, err := repo.Create(ctx, scope, "tenant", "acme", data)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == "" || c.Type != "tenant" || c.ExternalID != "acme" {
		t.Fatalf("unexpected created context: %+v", c)
	}

	got, err := repo.Get(ctx, scope, "tenant", "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("Get id mismatch: %q != %q", got.ID, c.ID)
	}

	updated, err := repo.Update(ctx, scope, "tenant", "acme", json.RawMessage(`{"plan":"pro"}`))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if string(updated.Data) != `{"plan":"pro"}` {
		t.Fatalf("Update data mismatch: %s", updated.Data)
	}

	if err := repo.Delete(ctx, scope, "tenant", "acme"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, scope, "tenant", "acme"); !isNotFound(err) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
}

func TestContextRepo_CompositeKeyConflict(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.Contexts()

	_, err := repo.Create(ctx, scope, "tenant", "dup", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err = repo.Create(ctx, scope, "tenant", "dup", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("second Create: want conflict error, got nil")
	}
	// Must be ErrContextConflict, not a raw SQLite error.
	if !isContextConflict(err) {
		t.Fatalf("second Create: want ErrContextConflict, got %v", err)
	}
}

func TestContextRepo_ScopeIsolation(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	s1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	s2 := domain.Scope{OrgID: "o2", EnvID: "e2"}
	repo := s.Contexts()

	_, err := repo.Create(ctx, s1, "tenant", "iso", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Create in s1: %v", err)
	}
	// s2 should NOT see s1's context.
	if _, err := repo.Get(ctx, s2, "tenant", "iso"); !isNotFound(err) {
		t.Fatalf("Get in s2: want ErrNotFound, got %v", err)
	}
}

func TestContextRepo_KeysetPagination(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.Contexts()

	// Create 5 contexts.
	for i := 0; i < 5; i++ {
		_, err := repo.Create(ctx, scope, "tenant", fmt.Sprintf("ctx%d", i), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// First page: limit 2.
	page1, next, _, err := repo.List(ctx, scope, "", "", "", "", 2)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if next == "" {
		t.Fatal("expected nextCursor on page1")
	}

	// Second page.
	page2, _, _, err := repo.List(ctx, scope, "", "", next, "", 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) == 0 {
		t.Fatal("page2 empty, expected items")
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("page2 same as page1")
	}
}

func TestContextRepo_NotFound(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.Contexts()

	if _, err := repo.Get(ctx, scope, "tenant", "ghost"); !isNotFound(err) {
		t.Fatalf("Get nonexistent: want ErrNotFound, got %v", err)
	}
	if err := repo.Delete(ctx, scope, "tenant", "ghost"); !isNotFound(err) {
		t.Fatalf("Delete nonexistent: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Update(ctx, scope, "tenant", "ghost", json.RawMessage(`{}`)); !isNotFound(err) {
		t.Fatalf("Update nonexistent: want ErrNotFound, got %v", err)
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

func isContextConflict(err error) bool {
	return errors.Is(err, ErrContextConflict)
}
