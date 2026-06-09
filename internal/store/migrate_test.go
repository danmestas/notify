package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
)

// openTemp opens a Store on a fresh temp-dir db path, registering cleanup.
func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_Migrate_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notify.db")

	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	count1 := migrationCount(t, s1)
	_ = s1.Close()

	// Re-open the same path: migrations already recorded must not re-apply.
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	count2 := migrationCount(t, s2)

	if count1 != count2 {
		t.Fatalf("schema_migrations count changed across re-open: %d -> %d", count1, count2)
	}
	if count1 == 0 {
		t.Fatalf("expected at least one recorded migration, got 0")
	}

	want := []string{
		"api_keys", "environments", "messages", "notifications",
		"organizations", "subscribers", "workflows",
	}
	got := businessTables(t, s2)
	if len(got) != len(want) {
		t.Fatalf("table set mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("table set mismatch: got %v want %v", got, want)
		}
	}
}

func migrationCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

func businessTables(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.reader.Query(
		`SELECT name FROM sqlite_master
		 WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'`,
	)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
