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
