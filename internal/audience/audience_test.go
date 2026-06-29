package audience_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func newSvc(t *testing.T) (*audience.Service, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return audience.New(s), ctx
}

func TestUpsertDedupesAndUpdates(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	first, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "ext-1",
		Email:        "a@example.com",
		FirstName:    "Ada",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.FirstName != "Ada" {
		t.Fatalf("first name = %q, want Ada", first.FirstName)
	}

	second, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "ext-1",
		Email:        "a@example.com",
		FirstName:    "Grace",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert created new row: id %q != %q", second.ID, first.ID)
	}
	if second.FirstName != "Grace" {
		t.Fatalf("first name = %q, want Grace", second.FirstName)
	}

	got, err := svc.Get(ctx, scope, "ext-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FirstName != "Grace" {
		t.Fatalf("get first name = %q, want Grace", got.FirstName)
	}
}

func TestGetReturnsUpserted(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if _, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "ext-2",
		Email:        "b@example.com",
		LastName:     "Lovelace",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := svc.Get(ctx, scope, "ext-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SubscriberID != "ext-2" || got.Email != "b@example.com" || got.LastName != "Lovelace" {
		t.Fatalf("get returned %+v", got)
	}
}

func TestScopeIsolation(t *testing.T) {
	svc, ctx := newSvc(t)
	e1 := domain.Scope{OrgID: "org1", EnvID: "e1"}
	e2 := domain.Scope{OrgID: "org1", EnvID: "e2"}

	if _, err := svc.Upsert(ctx, e1, audience.SubscriberInput{SubscriberID: "shared"}); err != nil {
		t.Fatalf("upsert e1: %v", err)
	}

	_, err := svc.Get(ctx, e2, "shared")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get under e2 = %v, want ErrNotFound", err)
	}
}

// TestNewlyCreatedSubscriberIsImmediatelyFindable is the Bug 5 investigation:
// a subscriber upserted via Upsert must be immediately returned by ListSubscribers
// and findable by name/email in the same process. There is no async indexing;
// the store uses SQLite WAL with a shared file and both reader and writer see
// the committed row synchronously.
func TestNewlyCreatedSubscriberIsImmediatelyFindable(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "new-sub-findme",
		Email:        "findme@example.com",
		FirstName:    "Find",
		LastName:     "Me",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Must be in ListSubscribers immediately after create.
	page, err := svc.ListSubscribers(ctx, scope, "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.TotalCount != 1 {
		t.Errorf("TotalCount = %d, want 1", page.TotalCount)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(page.Items))
	}
	if page.Items[0].ID != sub.ID {
		t.Errorf("item ID = %q, want %q", page.Items[0].ID, sub.ID)
	}

	// Must be findable by external ID immediately.
	got, err := svc.Get(ctx, scope, "new-sub-findme")
	if err != nil {
		t.Fatalf("Get by external id: %v", err)
	}
	if got.Email != "findme@example.com" {
		t.Errorf("email = %q, want findme@example.com", got.Email)
	}

	// Must be findable by internal ID immediately.
	gotByID, err := svc.GetSubscriberByID(ctx, scope, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscriberByID: %v", err)
	}
	if gotByID.FirstName != "Find" {
		t.Errorf("first name = %q, want Find", gotByID.FirstName)
	}
}

// TestGetSubscriberByID_SoftDeletedReturnsNotFound guards Bug 2:
// GetSubscriberByID must return ErrNotFound for a soft-deleted subscriber so
// the console detail page 404s instead of rendering the deleted record.
func TestGetSubscriberByID_SoftDeletedReturnsNotFound(t *testing.T) {
	svc, ctx := newSvc(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	sub, err := svc.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "del-ext-1",
		Email:        "delete-me@example.com",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := svc.DeleteSubscriber(ctx, scope, sub.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	_, err = svc.GetSubscriberByID(ctx, scope, sub.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSubscriberByID after delete = %v, want ErrNotFound", err)
	}
}
