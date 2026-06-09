package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestSubscriberRepo_Upsert_DedupesByExternalID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}

	first, err := repo.Upsert(ctx, scopeA, domain.Subscriber{
		SubscriberID: "sub-1", Email: "a@example.com", FirstName: "Ann",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, err := repo.Upsert(ctx, scopeA, domain.Subscriber{
		SubscriberID: "sub-1", Email: "b@example.com", FirstName: "Bob",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Same (EnvID, SubscriberID) -> same row id, updated fields.
	if first.ID != second.ID {
		t.Fatalf("expected stable id on dedup, got %q then %q", first.ID, second.ID)
	}
	if second.Email != "b@example.com" || second.FirstName != "Bob" {
		t.Fatalf("expected fields updated, got %+v", second)
	}

	list, err := repo.List(ctx, scopeA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 row after dedup, got %d", len(list))
	}

	// Same subscriberId under a different EnvID is a distinct row.
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}
	other, err := repo.Upsert(ctx, scopeB, domain.Subscriber{SubscriberID: "sub-1", Email: "c@example.com"})
	if err != nil {
		t.Fatalf("cross-env upsert: %v", err)
	}
	if other.ID == first.ID {
		t.Fatalf("cross-env subscriber must be distinct row, got shared id %q", other.ID)
	}
	if listB, _ := repo.List(ctx, scopeB); len(listB) != 1 {
		t.Fatalf("expected 1 row under scope B, got %d", len(listB))
	}
}

func TestSubscriberRepo_Scope_Isolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}

	if _, err := repo.Upsert(ctx, scopeA, domain.Subscriber{
		SubscriberID: "sub-1", Email: "a@example.com",
		Data: json.RawMessage(`{"k":"v"}`),
	}); err != nil {
		t.Fatalf("upsert under A: %v", err)
	}

	// Visible under its own scope.
	got, err := repo.GetByExternalID(ctx, scopeA, "sub-1")
	if err != nil {
		t.Fatalf("get under A: %v", err)
	}
	if got.Email != "a@example.com" {
		t.Fatalf("unexpected subscriber under A: %+v", got)
	}

	// Invisible under a different env scope.
	if _, err := repo.GetByExternalID(ctx, scopeB, "sub-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound under scope B, got %v", err)
	}
}
