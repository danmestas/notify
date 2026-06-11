package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0009_AppliesLayouts(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 11 {
		t.Fatalf("expected 11 recorded migrations, got %d", n)
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
