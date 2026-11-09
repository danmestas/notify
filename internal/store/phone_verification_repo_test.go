package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestSubscriberRepo_UpsertNeverVerifiesPhone(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	got, err := repo.Upsert(ctx, scope, domain.Subscriber{
		SubscriberID: "sub-1", Phone: "+15550001111", PhoneVerifiedAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.PhoneVerifiedAt != "" {
		t.Fatalf("PhoneVerifiedAt = %q after a plain upsert, want empty", got.PhoneVerifiedAt)
	}
}

func TestSubscriberRepo_VerifiedPhoneSurvivesSameNumberClearsOnChange(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.SetVerifiedPhone(ctx, scope, "sub-1", "+15550001111", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetVerifiedPhone: %v", err)
	}
	got, err := repo.GetByExternalID(ctx, scope, "sub-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Phone != "+15550001111" || got.PhoneVerifiedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("after verify: phone=%q verifiedAt=%q", got.Phone, got.PhoneVerifiedAt)
	}

	// A write that leaves the phone alone, or repeats it, keeps verification.
	if _, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", FirstName: "Ann"}); err != nil {
		t.Fatalf("upsert name: %v", err)
	}
	if _, err := repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+15550001111"}); err != nil {
		t.Fatalf("upsert same phone: %v", err)
	}
	got, _ = repo.GetByExternalID(ctx, scope, "sub-1")
	if got.PhoneVerifiedAt == "" {
		t.Fatal("verification cleared by a write that did not change the phone")
	}

	// A different number clears it.
	got, err = repo.Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Phone: "+15559998888"})
	if err != nil {
		t.Fatalf("upsert new phone: %v", err)
	}
	if got.Phone != "+15559998888" || got.PhoneVerifiedAt != "" {
		t.Fatalf("after phone change: phone=%q verifiedAt=%q, want new phone unverified", got.Phone, got.PhoneVerifiedAt)
	}
}

func TestSubscriberRepo_SetVerifiedPhone_UnknownSubscriber(t *testing.T) {
	s := openTemp(t)
	err := s.Subscribers().SetVerifiedPhone(context.Background(), domain.Scope{OrgID: "o1", EnvID: "e1"}, "ghost", "+15550001111", "2026-01-01T00:00:00Z")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPhoneVerificationRepo_LatestCountsAttemptsConsume(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.PhoneVerifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	for i, v := range []PhoneVerification{
		{ID: "01A", SubscriberID: "sub-1", Phone: "+15550001111", CodeHash: "h1", ExpiresAt: "2026-01-01T00:10:00Z", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "01B", SubscriberID: "sub-1", Phone: "+15550002222", CodeHash: "h2", ExpiresAt: "2026-01-01T00:11:00Z", CreatedAt: "2026-01-01T00:01:00Z"},
		{ID: "01C", SubscriberID: "sub-2", Phone: "+15550002222", CodeHash: "h3", ExpiresAt: "2026-01-01T00:12:00Z", CreatedAt: "2026-01-01T00:02:00Z"},
	} {
		if err := repo.Create(ctx, scope, v); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	latest, err := repo.Latest(ctx, scope, "sub-1")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.ID != "01B" || latest.CodeHash != "h2" {
		t.Fatalf("Latest = %+v, want 01B", latest)
	}
	if _, err := repo.Latest(ctx, other, "sub-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Latest in another env: err = %v, want ErrNotFound", err)
	}

	for i := 1; i <= 2; i++ {
		ok, err := repo.ClaimAttempt(ctx, scope, "01B", 2)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v, want ok", i, ok, err)
		}
	}
	if ok, _ := repo.ClaimAttempt(ctx, scope, "01B", 2); ok {
		t.Fatal("claim past the limit succeeded")
	}
	// 01C: one attempt that succeeded. A success is not a failure.
	if ok, _ := repo.ClaimAttempt(ctx, scope, "01C", 5); !ok {
		t.Fatal("claim 01C failed")
	}
	if ok, _ := repo.Consume(ctx, scope, "01C"); !ok {
		t.Fatal("consume 01C failed")
	}

	u, err := repo.Usage(ctx, scope, "sub-1", "+15550002222", "2026-01-01T00:00:30Z", "2025-12-31T01:00:00Z")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	want := PhoneVerificationUsage{SubscriberRecent: 1, PhoneRecent: 2, PhoneDay: 2, EnvDay: 3, PhoneFailedDay: 2}
	if u != want {
		t.Fatalf("Usage = %+v, want %+v", u, want)
	}
	// Day counts honour their own cutoff.
	u, _ = repo.Usage(ctx, scope, "sub-1", "+15550002222", "2026-01-01T00:00:30Z", "2026-01-01T00:01:30Z")
	if u.EnvDay != 1 || u.PhoneDay != 1 || u.PhoneFailedDay != 0 {
		t.Fatalf("Usage after 00:01:30 = %+v, want only 01C", u)
	}

	if ok, err := repo.Consume(ctx, scope, "01A"); err != nil || !ok {
		t.Fatalf("consume: ok=%v err=%v", ok, err)
	}
	if ok, _ := repo.Consume(ctx, scope, "01A"); ok {
		t.Fatal("second consume succeeded")
	}
	if ok, _ := repo.ClaimAttempt(ctx, scope, "01A", 5); ok {
		t.Fatal("claim on a consumed code succeeded")
	}
}
