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

	page, err := repo.List(ctx, scopeA, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 row after dedup, got %d", len(page.Items))
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
	if pageB, _ := repo.List(ctx, scopeB, "", 0); len(pageB.Items) != 1 {
		t.Fatalf("expected 1 row under scope B, got %d", len(pageB.Items))
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

func TestSubscriberRepo_List_KeysetCursor(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Insert 5 subscribers.
	for i := range 5 {
		id := []string{"a", "b", "c", "d", "e"}[i]
		if _, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-" + id, Email: id + "@x.com"}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	// Page 1: limit 2.
	p1, err := repo.List(ctx, scope, "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(p1.Items) != 2 {
		t.Fatalf("page1 items = %d, want 2", len(p1.Items))
	}
	if p1.TotalCount != 5 {
		t.Fatalf("page1 total = %d, want 5", p1.TotalCount)
	}
	if p1.NextCursor == "" {
		t.Fatalf("page1 expected non-empty NextCursor")
	}

	// Page 2: limit 2, cursor from page 1.
	p2, err := repo.List(ctx, scope, p1.NextCursor, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(p2.Items) != 2 {
		t.Fatalf("page2 items = %d, want 2", len(p2.Items))
	}
	if p2.NextCursor == "" {
		t.Fatalf("page2 expected non-empty NextCursor")
	}

	// Page 3: last page, only 1 item.
	p3, err := repo.List(ctx, scope, p2.NextCursor, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(p3.Items) != 1 {
		t.Fatalf("page3 items = %d, want 1", len(p3.Items))
	}
	if p3.NextCursor != "" {
		t.Fatalf("page3 expected empty NextCursor, got %q", p3.NextCursor)
	}

	// No duplicates across pages.
	seen := map[string]bool{}
	for _, pg := range []SubscriberPage{p1, p2, p3} {
		for _, sub := range pg.Items {
			if seen[sub.ID] {
				t.Errorf("duplicate id %q across pages", sub.ID)
			}
			seen[sub.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Errorf("total across pages = %d, want 5", len(seen))
	}
}

func TestSubscriberRepo_List_ExcludesDeleted(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	sub, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "alive", Email: "a@x.com"})
	if err != nil {
		t.Fatalf("upsert alive: %v", err)
	}
	if _, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "dead", Email: "d@x.com"}); err != nil {
		t.Fatalf("upsert dead: %v", err)
	}
	// Soft-delete "dead" via SoftDelete by getting its id first.
	dead, _ := repo.GetByExternalID(ctx, scope, "dead")
	if err := repo.SoftDelete(ctx, scope, dead.ID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	page, err := repo.List(ctx, scope, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.TotalCount != 1 {
		t.Fatalf("total = %d, want 1", page.TotalCount)
	}
	if len(page.Items) != 1 || page.Items[0].ID != sub.ID {
		t.Fatalf("expected only alive subscriber, got %+v", page.Items)
	}
}

func TestSubscriberRepo_GetByID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	sub, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "s1", Email: "x@y.com"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.GetByID(ctx, scope, sub.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SubscriberID != "s1" {
		t.Errorf("SubscriberID = %q, want s1", got.SubscriberID)
	}

	// Cross-scope lookup must return ErrNotFound.
	if _, err := repo.GetByID(ctx, other, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-scope GetByID = %v, want ErrNotFound", err)
	}
}

func TestSubscriberRepo_SoftDelete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	sub, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "s1", Email: "x@y.com"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := repo.SoftDelete(ctx, scope, sub.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	// Deleted once, 2nd call returns ErrNotFound.
	if err := repo.SoftDelete(ctx, scope, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second SoftDelete = %v, want ErrNotFound", err)
	}

	// GetByID still finds the row (it's soft-deleted, not gone).
	got, err := repo.GetByID(ctx, scope, sub.ID)
	if err != nil {
		t.Fatalf("GetByID after soft-delete: %v", err)
	}
	if !got.Deleted {
		t.Errorf("expected Deleted=true, got %+v", got)
	}

	// List excludes it.
	page, _ := repo.List(ctx, scope, "", 0)
	if len(page.Items) != 0 {
		t.Errorf("expected empty page, got %d items", len(page.Items))
	}
}
