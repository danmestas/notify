package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func openWebPushStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWebPushSubscriptions_TwoDevicesTwoRows(t *testing.T) {
	ctx := context.Background()
	s := openWebPushStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: "user_1",
		Endpoint:     "https://fcm.googleapis.com/fcm/send/device-a",
		P256dh:       "p256dh-a",
		Auth:         "auth-a",
	}); err != nil {
		t.Fatalf("UpsertByEndpoint device a: %v", err)
	}
	if _, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: "user_1",
		Endpoint:     "https://fcm.googleapis.com/fcm/send/device-b",
		P256dh:       "p256dh-b",
		Auth:         "auth-b",
	}); err != nil {
		t.Fatalf("UpsertByEndpoint device b: %v", err)
	}

	got, err := s.WebPushSubscriptions().ListBySubscriber(ctx, scope, "user_1")
	if err != nil {
		t.Fatalf("ListBySubscriber: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
}

func TestWebPushSubscriptions_SameEndpointTwiceUpdatesOneRow(t *testing.T) {
	ctx := context.Background()
	s := openWebPushStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	first, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: "user_1",
		Endpoint:     "https://fcm.googleapis.com/fcm/send/device-a",
		P256dh:       "p256dh-old",
		Auth:         "auth-old",
	})
	if err != nil {
		t.Fatalf("first UpsertByEndpoint: %v", err)
	}

	second, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: "user_1",
		Endpoint:     "https://fcm.googleapis.com/fcm/send/device-a",
		P256dh:       "p256dh-new",
		Auth:         "auth-new",
	})
	if err != nil {
		t.Fatalf("second UpsertByEndpoint: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("ID changed on re-upsert: got %q, want %q (same row)", second.ID, first.ID)
	}
	if second.P256dh != "p256dh-new" || second.Auth != "auth-new" {
		t.Errorf("keys not refreshed: got p256dh=%q auth=%q", second.P256dh, second.Auth)
	}

	got, err := s.WebPushSubscriptions().ListBySubscriber(ctx, scope, "user_1")
	if err != nil {
		t.Fatalf("ListBySubscriber: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (same endpoint updates in place)", len(got))
	}
}

func TestWebPushSubscriptions_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openWebPushStore(t)
	scopeA := domain.Scope{OrgID: "org-a", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "org-b", EnvID: "e1"}

	if _, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scopeA, domain.WebPushSubscription{
		SubscriberID: "user_1",
		Endpoint:     "https://fcm.googleapis.com/fcm/send/device-a",
		P256dh:       "p256dh-a",
		Auth:         "auth-a",
	}); err != nil {
		t.Fatalf("UpsertByEndpoint scope A: %v", err)
	}

	got, err := s.WebPushSubscriptions().ListBySubscriber(ctx, scopeB, "user_1")
	if err != nil {
		t.Fatalf("ListBySubscriber scope B: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("org-b saw %d rows belonging to org-a, want 0", len(got))
	}
}

func TestWebPushSubscriptions_DeleteByEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openWebPushStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	endpoint := "https://fcm.googleapis.com/fcm/send/device-a"

	if _, err := s.WebPushSubscriptions().UpsertByEndpoint(ctx, scope, domain.WebPushSubscription{
		SubscriberID: "user_1",
		Endpoint:     endpoint,
		P256dh:       "p256dh-a",
		Auth:         "auth-a",
	}); err != nil {
		t.Fatalf("UpsertByEndpoint: %v", err)
	}

	if err := s.WebPushSubscriptions().DeleteByEndpoint(ctx, scope, "user_1", endpoint); err != nil {
		t.Fatalf("DeleteByEndpoint: %v", err)
	}

	got, err := s.WebPushSubscriptions().ListBySubscriber(ctx, scope, "user_1")
	if err != nil {
		t.Fatalf("ListBySubscriber: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0 after delete", len(got))
	}

	if err := s.WebPushSubscriptions().DeleteByEndpoint(ctx, scope, "user_1", endpoint); err != ErrNotFound {
		t.Fatalf("DeleteByEndpoint on already-deleted row: err = %v, want ErrNotFound", err)
	}
}
