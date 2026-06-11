package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0007_AppliesTopics(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 10 {
		t.Fatalf("expected 10 recorded migrations, got %d", n)
	}

	for _, table := range []string{"topics", "topic_subscribers"} {
		var name string
		err := s.reader.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("%s table missing: %v", table, err)
		}
	}
}

func TestTopicRepo_CreateAddList(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Topics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	topic, err := repo.Create(ctx, scope, "vip", "VIP customers")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if topic.ID == "" || topic.Key != "vip" || topic.Name != "VIP customers" {
		t.Fatalf("unexpected topic: %+v", topic)
	}

	if err := repo.AddSubscriber(ctx, scope, "vip", "sub-a"); err != nil {
		t.Fatalf("add sub-a: %v", err)
	}
	if err := repo.AddSubscriber(ctx, scope, "vip", "sub-b"); err != nil {
		t.Fatalf("add sub-b: %v", err)
	}

	ids, err := repo.ListSubscriberIDs(ctx, scope, "vip")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 2 || ids[0] != "sub-a" || ids[1] != "sub-b" {
		t.Fatalf("expected [sub-a sub-b], got %v", ids)
	}

	// Duplicate AddSubscriber is idempotent (INSERT OR IGNORE).
	if err := repo.AddSubscriber(ctx, scope, "vip", "sub-a"); err != nil {
		t.Fatalf("duplicate add: %v", err)
	}
	ids, err = repo.ListSubscriberIDs(ctx, scope, "vip")
	if err != nil {
		t.Fatalf("list after dup: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("duplicate add changed membership: %v", ids)
	}

	// AddSubscriber to an unknown topic key -> ErrNotFound.
	if err := repo.AddSubscriber(ctx, scope, "ghost", "sub-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown topic, got %v", err)
	}

	// Cross-env isolation: topic/membership under e1 invisible to e2.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if _, err := repo.GetByKey(ctx, other, "vip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for e2 GetByKey, got %v", err)
	}
	otherIDs, err := repo.ListSubscriberIDs(ctx, other, "vip")
	if err != nil {
		t.Fatalf("list e2: %v", err)
	}
	if len(otherIDs) != 0 {
		t.Fatalf("e2 must not see e1 membership, got %v", otherIDs)
	}
}
