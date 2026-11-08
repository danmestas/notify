package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestSubscriberRepo_PhoneIsNormalisedSoFormatsDoNotClearVerification: every
// write stores E.164, so a trigger repeating the verified number in another
// format keeps it verified, and only a different number clears it.
func TestSubscriberRepo_PhoneIsNormalisedSoFormatsDoNotClearVerification(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	got, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+1 (555) 111-2222"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.Phone != "+15551112222" {
		t.Fatalf("stored phone = %q, want E.164", got.Phone)
	}
	if err := repo.SetVerifiedPhone(ctx, scope, "sub-1", "+15551112222", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetVerifiedPhone: %v", err)
	}
	for _, same := range []string{"+1 (555) 111-2222", "+1.555.111.2222", " +15551112222 "} {
		got, err = repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: same})
		if err != nil {
			t.Fatalf("upsert %q: %v", same, err)
		}
		if got.PhoneVerifiedAt == "" || got.Phone != "+15551112222" {
			t.Fatalf("after %q: %+v, want the same number still verified", same, got)
		}
	}
	got, _ = repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+1 555 111 3333"})
	if got.PhoneVerifiedAt != "" || got.Phone != "+15551113333" {
		t.Fatalf("after a different number: %+v, want it stored unverified", got)
	}

	// What cannot be normalised is stored as given, unverified.
	got, _ = repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-2", Phone: "555-1234"})
	if got.Phone != "555-1234" || got.PhoneVerifiedAt != "" {
		t.Fatalf("un-normalisable phone: %+v, want stored as given", got)
	}
}

// TestSubscriberRepo_SoftDeleteDropsVerification: a deleted subscriber keeps
// no verified phone and no code it can still confirm.
func TestSubscriberRepo_SoftDeleteDropsVerification(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	sub, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Subscribers().SetVerifiedPhone(ctx, scope, "sub-1", "+15551112222", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetVerifiedPhone: %v", err)
	}
	if err := s.PhoneVerifications().Create(ctx, scope, PhoneVerification{
		ID: "pv1", SubscriberID: "sub-1", Phone: "+15551112222", CodeHash: "h",
		ExpiresAt: "2099-01-01T00:00:00Z", CreatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("create code: %v", err)
	}

	if err := s.Subscribers().SoftDelete(ctx, scope, sub.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	got, err := s.Subscribers().GetByID(ctx, scope, sub.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PhoneVerifiedAt != "" {
		t.Errorf("deleted subscriber still verified: %+v", got)
	}
	pending, err := s.PhoneVerifications().Latest(ctx, scope, "sub-1")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if !pending.Consumed {
		t.Error("pending code still confirmable after delete")
	}
	// The send stays counted toward the number's limits.
	if u, _ := s.PhoneVerifications().Usage(ctx, scope, "sub-1", "+15551112222", "2000-01-01T00:00:00Z", "2000-01-01T00:00:00Z"); u.PhoneDay != 1 {
		t.Errorf("Usage after delete = %+v, want the send still counted", u)
	}
}
