package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0013_AppliesEnvVars(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 29 {
		t.Fatalf("expected 27 recorded migrations, got %d", n)
	}

	for _, tbl := range []string{"env_vars", "env_var_values"} {
		var name string
		err := s.reader.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}

func TestEnvVarRepo_CRUD(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.EnvVars()

	enc := []byte("fake-ciphertext-123")

	// Upsert (insert new).
	ev, err := repo.Upsert(ctx, scope, "API_KEY", true, enc)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if ev.ID == "" || ev.Key != "API_KEY" || !ev.IsSecret {
		t.Fatalf("unexpected upserted: %+v", ev)
	}

	// Get metadata.
	got, err := repo.Get(ctx, scope, "API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != ev.ID || got.Key != "API_KEY" {
		t.Fatalf("Get mismatch: %+v", got)
	}

	// GetValue.
	val, err := repo.GetValue(ctx, scope, "API_KEY")
	if err != nil {
		t.Fatalf("GetValue: %v", err)
	}
	if string(val) != string(enc) {
		t.Fatalf("GetValue mismatch: got %q, want %q", val, enc)
	}

	// List.
	items, _, _, err := repo.List(ctx, scope, "", "", "", 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List len = %d, want 1", len(items))
	}

	// Delete.
	if err := repo.Delete(ctx, scope, "API_KEY"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, scope, "API_KEY"); !isNotFound(err) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
}

func TestEnvVarRepo_KeyConflictUpserts(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.EnvVars()

	enc1 := []byte("value-one")
	enc2 := []byte("value-two")

	ev1, err := repo.Upsert(ctx, scope, "SAME_KEY", true, enc1)
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Second upsert for same key must succeed (not error) and update value.
	ev2, err := repo.Upsert(ctx, scope, "SAME_KEY", false, enc2)
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	// ID should be the same row.
	if ev1.ID != ev2.ID {
		t.Fatalf("upsert changed ID: %q != %q", ev1.ID, ev2.ID)
	}
	// IsSecret should be updated.
	if ev2.IsSecret {
		t.Fatalf("upsert did not update isSecret")
	}

	// Value should be updated.
	val, err := repo.GetValue(ctx, scope, "SAME_KEY")
	if err != nil {
		t.Fatalf("GetValue after upsert: %v", err)
	}
	if string(val) != string(enc2) {
		t.Fatalf("GetValue after upsert: got %q, want %q", val, enc2)
	}

	// Only one row in list.
	items, _, _, err := repo.List(ctx, scope, "", "", "", 50)
	if err != nil {
		t.Fatalf("List after upsert: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List after upsert: want 1 item, got %d", len(items))
	}
}

func TestEnvVarRepo_ScopeIsolation(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	s1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	s2 := domain.Scope{OrgID: "o2", EnvID: "e2"}
	repo := s.EnvVars()

	_, err := repo.Upsert(ctx, s1, "SECRET", true, []byte("for-scope1"))
	if err != nil {
		t.Fatalf("Upsert in s1: %v", err)
	}

	// s2 should NOT see s1's variable.
	if _, err := repo.Get(ctx, s2, "SECRET"); !isNotFound(err) {
		t.Fatalf("Get in s2: want ErrNotFound, got %v", err)
	}
	if _, err := repo.GetValue(ctx, s2, "SECRET"); !isNotFound(err) {
		t.Fatalf("GetValue in s2: want ErrNotFound, got %v", err)
	}

	// s2 list must be empty.
	items, _, _, err := repo.List(ctx, s2, "", "", "", 50)
	if err != nil {
		t.Fatalf("List in s2: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("s2 List: want 0 items, got %d", len(items))
	}
}

func TestEnvVarRepo_NotFound(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.EnvVars()

	if _, err := repo.Get(ctx, scope, "GHOST"); !isNotFound(err) {
		t.Fatalf("Get nonexistent: want ErrNotFound, got %v", err)
	}
	if _, err := repo.GetValue(ctx, scope, "GHOST"); !isNotFound(err) {
		t.Fatalf("GetValue nonexistent: want ErrNotFound, got %v", err)
	}
	if err := repo.Delete(ctx, scope, "GHOST"); !isNotFound(err) {
		t.Fatalf("Delete nonexistent: want ErrNotFound, got %v", err)
	}
}
