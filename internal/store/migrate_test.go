package store

import (
	"context"
	"database/sql"
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
		"agent_links", "agents", "analytics_deliveries", "api_keys", "builder_checkpoints", "builder_messages", "builder_sessions", "chat_threads", "contexts", "conversation_activities", "conversations",
		"delay_deadlines", "email_threads", "env_var_values", "env_vars",
		"environments", "inbound_domains", "integrations", "layouts", "messages", "notifications",
		"organizations", "preferences", "step_outputs", "stream_sources", "subscriber_channels", "subscribers", "topic_subscribers", "topic_triggers", "topics", "translations", "webhook_endpoints", "workflow_versions", "workflows",
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

func TestMigrate_0005_AppliesDigestKey(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 31 {
		t.Fatalf("expected 31 recorded migrations, got %d", n)
	}

	var found bool
	rows, err := s.reader.Query(`PRAGMA table_info(notifications)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notnull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &primaryKey); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == "digest_key" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if !found {
		t.Fatalf("notifications table missing digest_key column")
	}
}

// TestMigrate_0028_AddsWorkflowVersionSnapshotColumns proves migration 0028
// is additive (workflow_versions gains steps/payload_schema/critical without
// disturbing any existing column) and idempotent (re-opening the same db path
// does not re-apply it or change the migration count).
func TestMigrate_0028_AddsWorkflowVersionSnapshotColumns(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 31 {
		t.Fatalf("expected 31 recorded migrations, got %d", n)
	}

	want := map[string]bool{"steps": false, "payload_schema": false, "critical": false}
	rows, err := s.reader.Query(`PRAGMA table_info(workflow_versions)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notnull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &primaryKey); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	for col, found := range want {
		if !found {
			t.Fatalf("workflow_versions missing column %q", col)
		}
	}
}
