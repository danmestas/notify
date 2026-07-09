package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0002_AppliesIntegrations(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 27 {
		t.Fatalf("expected 26 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='integrations'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("integrations table missing: %v", err)
	}
}

func TestIntegrationRepo_GetActiveByChannel_PrefersPrimary(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Non-primary with lower priority value (would win on priority alone).
	if _, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true,
		Primary: false, Priority: 0, Identifier: "smtp-secondary",
	}); err != nil {
		t.Fatalf("create secondary: %v", err)
	}
	primary, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "capture", Channel: "email", Active: true,
		Primary: true, Priority: 5, Identifier: "capture-primary",
	})
	if err != nil {
		t.Fatalf("create primary: %v", err)
	}

	got, err := repo.GetActiveByChannel(ctx, scope, "email")
	if err != nil {
		t.Fatalf("GetActiveByChannel: %v", err)
	}
	if got.ID != primary.ID {
		t.Fatalf("expected primary %q, got %q", primary.ID, got.ID)
	}
	if !got.Primary {
		t.Fatalf("expected primary_provider=1, got %+v", got)
	}
}

func TestIntegrationRepo_GetActiveByChannel_PriorityThenScope(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	// No primary: lowest priority wins.
	if _, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true,
		Priority: 10, Identifier: "high-prio",
	}); err != nil {
		t.Fatalf("create high-prio: %v", err)
	}
	low, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "capture", Channel: "email", Active: true,
		Priority: 1, Identifier: "low-prio",
	})
	if err != nil {
		t.Fatalf("create low-prio: %v", err)
	}
	// Inactive must be excluded even with the lowest priority.
	if _, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: false,
		Priority: 0, Identifier: "inactive",
	}); err != nil {
		t.Fatalf("create inactive: %v", err)
	}

	got, err := repo.GetActiveByChannel(ctx, e1, "email")
	if err != nil {
		t.Fatalf("GetActiveByChannel e1: %v", err)
	}
	if got.ID != low.ID {
		t.Fatalf("expected lowest priority %q, got %q", low.ID, got.ID)
	}

	// Scope isolation: an integration under e2 only is invisible to e1; and e1
	// rows are invisible to e2.
	if _, err := repo.Create(ctx, e2, domain.Integration{
		ProviderID: "smtp", Channel: "sms", Active: true,
		Priority: 0, Identifier: "e2-only",
	}); err != nil {
		t.Fatalf("create e2-only: %v", err)
	}
	if _, err := repo.GetActiveByChannel(ctx, e1, "sms"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for sms under e1, got %v", err)
	}
	if _, err := repo.GetActiveByChannel(ctx, e2, "email"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for email under e2, got %v", err)
	}
}

// --- Wave 2 CRUD tests ---

func TestIntegrationRepo_List_NeverReturnsEncCredentials(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	enc := []byte("super-secret-ciphertext-bytes")
	if _, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true,
		EncCredentials: enc, Identifier: "smtp-1",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := repo.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(list))
	}
	if len(list[0].EncCredentials) != 0 {
		t.Errorf("List returned EncCredentials — must be nil/empty for security")
	}
}

func TestIntegrationRepo_List_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	if _, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-e1",
	}); err != nil {
		t.Fatalf("create e1: %v", err)
	}
	if _, err := repo.Create(ctx, e2, domain.Integration{
		ProviderID: "ntfy", Channel: "push", Active: true, Identifier: "ntfy-e2",
	}); err != nil {
		t.Fatalf("create e2: %v", err)
	}

	list1, err := repo.List(ctx, e1)
	if err != nil {
		t.Fatalf("List e1: %v", err)
	}
	if len(list1) != 1 || list1[0].Identifier != "smtp-e1" {
		t.Errorf("e1 list = %+v, want 1 smtp-e1 row", list1)
	}

	list2, err := repo.List(ctx, e2)
	if err != nil {
		t.Fatalf("List e2: %v", err)
	}
	if len(list2) != 1 || list2[0].Identifier != "ntfy-e2" {
		t.Errorf("e2 list = %+v, want 1 ntfy-e2 row", list2)
	}
}

func TestIntegrationRepo_GetByID_NotFoundOrWrongScope(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	row, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-e1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Correct scope must succeed.
	got, err := repo.GetByID(ctx, e1, row.ID)
	if err != nil {
		t.Fatalf("GetByID correct scope: %v", err)
	}
	if got.ID != row.ID {
		t.Errorf("got id %q, want %q", got.ID, row.ID)
	}

	// Wrong scope must return ErrNotFound (no information leakage).
	if _, err := repo.GetByID(ctx, e2, row.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID wrong scope: want ErrNotFound, got %v", err)
	}

	// Nonexistent id must return ErrNotFound.
	if _, err := repo.GetByID(ctx, e1, "nosuchid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID nonexistent: want ErrNotFound, got %v", err)
	}
}

func TestIntegrationRepo_Update_MutableFields(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	row, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true,
		Primary: false, Priority: 0, Identifier: "original",
		EncCredentials: []byte("old-ciphertext"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated := row
	updated.Identifier = "updated-name"
	updated.Primary = true
	updated.Priority = 5
	updated.EncCredentials = []byte("new-ciphertext")

	if err := repo.Update(ctx, scope, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, scope, row.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Identifier != "updated-name" {
		t.Errorf("Identifier = %q, want updated-name", got.Identifier)
	}
	if !got.Primary {
		t.Errorf("Primary = false, want true")
	}
	if got.Priority != 5 {
		t.Errorf("Priority = %d, want 5", got.Priority)
	}
	if string(got.EncCredentials) != "new-ciphertext" {
		t.Errorf("EncCredentials not updated")
	}
	// Immutable fields must not change.
	if got.ProviderID != "smtp" {
		t.Errorf("ProviderID mutated to %q", got.ProviderID)
	}
	if got.Channel != "email" {
		t.Errorf("Channel mutated to %q", got.Channel)
	}
}

func TestIntegrationRepo_Update_WrongScope_ReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	row, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-e1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	row.Identifier = "hack-attempt"
	if err := repo.Update(ctx, e2, row); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update wrong scope: want ErrNotFound, got %v", err)
	}
}

func TestIntegrationRepo_Delete_RemovesRow(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	row, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "to-delete",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, scope, row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, scope, row.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByID after delete: want ErrNotFound, got %v", err)
	}
}

func TestIntegrationRepo_Delete_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if err := repo.Delete(ctx, scope, "nosuchid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete nonexistent: want ErrNotFound, got %v", err)
	}
}

func TestIntegrationRepo_Delete_WrongScope_ReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	row, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-e1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, e2, row.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete wrong scope: want ErrNotFound, got %v", err)
	}
	// Row must still exist under e1.
	if _, err := repo.GetByID(ctx, e1, row.ID); err != nil {
		t.Errorf("Row was deleted by wrong-scope Delete: %v", err)
	}
}

func TestIntegrationRepo_SetActive_FlipsFlag(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	row, err := repo.Create(ctx, scope, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repo.SetActive(ctx, scope, row.ID, false); err != nil {
		t.Fatalf("SetActive false: %v", err)
	}
	got, _ := repo.GetByID(ctx, scope, row.ID)
	if got.Active {
		t.Error("Expected Active=false after SetActive(false)")
	}

	if err := repo.SetActive(ctx, scope, row.ID, true); err != nil {
		t.Fatalf("SetActive true: %v", err)
	}
	got, _ = repo.GetByID(ctx, scope, row.ID)
	if !got.Active {
		t.Error("Expected Active=true after SetActive(true)")
	}
}

func TestIntegrationRepo_SetActive_WrongScope_ReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Integrations()
	e1 := domain.Scope{OrgID: "o1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "e2"}

	row, err := repo.Create(ctx, e1, domain.Integration{
		ProviderID: "smtp", Channel: "email", Active: true, Identifier: "smtp-e1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetActive(ctx, e2, row.ID, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetActive wrong scope: want ErrNotFound, got %v", err)
	}
}
