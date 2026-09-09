package store

import "testing"

func TestMigrate_0010_AppliesAgents(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 35 {
		t.Fatalf("expected 35 recorded migrations, got %d", n)
	}

	for _, table := range []string{"agents", "agent_links", "conversations", "conversation_activities"} {
		var name string
		err := s.reader.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("%s table missing: %v", table, err)
		}
	}
}
