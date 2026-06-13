package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0009_AppliesLayouts(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 16 {
		t.Fatalf("expected 16 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "layouts",
	).Scan(&name)
	if err != nil {
		t.Fatalf("layouts table missing: %v", err)
	}
}

func TestLayoutRepo_UpsertGet(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Layouts()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Upsert(ctx, scope, "default", "Default", "<h/>{{content}}<f/>"); err != nil {
		t.Fatalf("upsert default: %v", err)
	}

	got, err := repo.Get(ctx, scope, "default")
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if got.Name != "Default" || got.Content != "<h/>{{content}}<f/>" || got.Key != "default" {
		t.Fatalf("unexpected layout: %+v", got)
	}

	// Upsert on the same (env, key) updates name+content in place.
	if err := repo.Upsert(ctx, scope, "default", "Renamed", "[ {{content}} ]"); err != nil {
		t.Fatalf("re-upsert default: %v", err)
	}
	got, err = repo.Get(ctx, scope, "default")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "Renamed" || got.Content != "[ {{content}} ]" {
		t.Fatalf("expected updated layout, got %+v", got)
	}

	// Cross-env isolation: e1 layout invisible under e2.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if _, err := repo.Get(ctx, other, "default"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("e2 must not see e1 layout, got err=%v", err)
	}

	// Unknown key yields ErrNotFound.
	if _, err := repo.Get(ctx, scope, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing key, got %v", err)
	}
}

func TestLayoutRepo_Create_ConflictOnDuplicate(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Layouts()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Seed via Upsert (the engine/render path), then Create the same key.
	if err := repo.Upsert(ctx, scope, "default", "Default", "{{content}}"); err != nil {
		t.Fatalf("upsert default: %v", err)
	}

	if _, err := repo.Create(ctx, scope, "default", "Dup", "{{content}}"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Create on duplicate key: err=%v, want ErrConflict", err)
	}

	// A fresh key in the same scope creates cleanly.
	got, err := repo.Create(ctx, scope, "fresh", "Fresh", "<x>{{content}}</x>")
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}
	if got.Key != "fresh" || got.Name != "Fresh" || got.Content != "<x>{{content}}</x>" {
		t.Fatalf("unexpected created layout: %+v", got)
	}
	if got.ID == "" {
		t.Fatalf("created layout missing id")
	}
	if got.OrgID != scope.OrgID || got.EnvID != scope.EnvID {
		t.Fatalf("created layout scope = (%s,%s), want (%s,%s)", got.OrgID, got.EnvID, scope.OrgID, scope.EnvID)
	}
}

func TestLayoutRepo_List_Keyset(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Layouts()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	keys := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for _, k := range keys {
		if _, err := repo.Create(ctx, scope, k, "N-"+k, "{{content}}"); err != nil {
			t.Fatalf("create %q: %v", k, err)
		}
	}

	// First page: limit 3.
	page1, next1, err := repo.List(ctx, scope, "", 3)
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1))
	}
	if next1 == "" {
		t.Fatalf("page1 nextKey empty, want a cursor")
	}
	// Keyset ordering is key ASC.
	if page1[0].Key != "alpha" || page1[1].Key != "bravo" || page1[2].Key != "charlie" {
		t.Fatalf("page1 keys = %s,%s,%s, want alpha,bravo,charlie", page1[0].Key, page1[1].Key, page1[2].Key)
	}

	// Second page from nextKey.
	page2, next2, err := repo.List(ctx, scope, next1, 3)
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	if next2 != "" {
		t.Fatalf("page2 nextKey = %q, want empty (last page)", next2)
	}

	// No overlap, full coverage.
	seen := map[string]int{}
	for _, l := range append(append([]domain.Layout{}, page1...), page2...) {
		seen[l.Key]++
	}
	if len(seen) != len(keys) {
		t.Fatalf("covered %d distinct keys, want %d", len(seen), len(keys))
	}
	for _, k := range keys {
		if seen[k] != 1 {
			t.Fatalf("key %q appeared %d times across pages, want exactly 1", k, seen[k])
		}
	}
}

func TestLayoutRepo_Delete_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Layouts()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Delete(ctx, scope, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete nonexistent: err=%v, want ErrNotFound", err)
	}
}

func TestLayoutRepo_Delete_Removes(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Layouts()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Create(ctx, scope, "doomed", "Doomed", "{{content}}"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, scope, "doomed"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, scope, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: err=%v, want ErrNotFound", err)
	}
}

func TestLayoutRepo_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Layouts()
	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	if _, err := repo.Create(ctx, scopeA, "shared", "A's layout", "{{content}}"); err != nil {
		t.Fatalf("create in scopeA: %v", err)
	}

	// List in scopeB sees nothing.
	rows, next, err := repo.List(ctx, scopeB, "", 50)
	if err != nil {
		t.Fatalf("list scopeB: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("scopeB list = %d rows, want 0", len(rows))
	}
	if next != "" {
		t.Fatalf("scopeB nextKey = %q, want empty", next)
	}

	// The same key is free to create in scopeB (per-env uniqueness).
	if _, err := repo.Create(ctx, scopeB, "shared", "B's layout", "{{content}}"); err != nil {
		t.Fatalf("create same key in scopeB: %v", err)
	}
}
