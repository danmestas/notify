package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0003_AppliesPreferences(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 15 {
		t.Fatalf("expected 15 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='preferences'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("preferences table missing: %v", err)
	}
}

func TestPreferenceRepo_Upsert_GetGlobal(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Preferences()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	saved, err := repo.Upsert(ctx, scope, domain.Preference{
		Type:         "subscriberGlobal",
		SubscriberID: "sub1",
		Channels:     []byte(`{"in_app":false}`),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if saved.ID == "" {
		t.Fatalf("expected minted id, got empty")
	}

	got, err := repo.GetGlobal(ctx, scope, "sub1")
	if err != nil {
		t.Fatalf("GetGlobal: %v", err)
	}
	if got.ID != saved.ID {
		t.Fatalf("id mismatch: got %q want %q", got.ID, saved.ID)
	}
	if !bytes.Equal(got.Channels, []byte(`{"in_app":false}`)) {
		t.Fatalf("channels mismatch: got %s", got.Channels)
	}

	// Re-Upsert updates channels in place (same unique key -> one row).
	if _, err := repo.Upsert(ctx, scope, domain.Preference{
		Type:         "subscriberGlobal",
		SubscriberID: "sub1",
		Channels:     []byte(`{"in_app":true,"email":false}`),
	}); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}

	got, err = repo.GetGlobal(ctx, scope, "sub1")
	if err != nil {
		t.Fatalf("GetGlobal after re-Upsert: %v", err)
	}
	if !bytes.Equal(got.Channels, []byte(`{"in_app":true,"email":false}`)) {
		t.Fatalf("channels not updated: got %s", got.Channels)
	}

	var n int
	if err := s.reader.QueryRow(
		`SELECT COUNT(*) FROM preferences WHERE environment_id='e1' AND subscriber_id='sub1'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row after re-Upsert, got %d", n)
	}
}

func TestPreferenceRepo_GetForWorkflow_Scoped(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Preferences()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Upsert(ctx, scope, domain.Preference{
		Type:         "workflow",
		SubscriberID: "sub1",
		WorkflowID:   "wf1",
		Channels:     []byte(`{"email":true}`),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetForWorkflow(ctx, scope, "sub1", "wf1")
	if err != nil {
		t.Fatalf("GetForWorkflow: %v", err)
	}
	if !bytes.Equal(got.Channels, []byte(`{"email":true}`)) {
		t.Fatalf("channels mismatch: got %s", got.Channels)
	}

	// A different environment scope must not see it (tenant isolation).
	otherScope := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if _, err := repo.GetForWorkflow(ctx, otherScope, "sub1", "wf1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for other env, got %v", err)
	}

	// Unset (sub, wf) returns ErrNotFound.
	if _, err := repo.GetForWorkflow(ctx, scope, "sub1", "wf-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unset workflow, got %v", err)
	}
}
