package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0007_AppliesTopics(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 17 {
		t.Fatalf("expected 17 recorded migrations, got %d", n)
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

func TestTopicRepo_List_WithMemberCount(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Topics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Create(ctx, scope, "alpha", "Alpha"); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := repo.Create(ctx, scope, "beta", "Beta"); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if err := repo.AddSubscriber(ctx, scope, "alpha", "sub-1"); err != nil {
		t.Fatalf("add sub-1 to alpha: %v", err)
	}
	if err := repo.AddSubscriber(ctx, scope, "alpha", "sub-2"); err != nil {
		t.Fatalf("add sub-2 to alpha: %v", err)
	}

	list, err := repo.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(list))
	}
	// Ordered by key ASC: alpha first.
	if list[0].Key != "alpha" {
		t.Errorf("list[0].Key = %q, want alpha", list[0].Key)
	}
	if list[0].MemberCount != 2 {
		t.Errorf("alpha MemberCount = %d, want 2", list[0].MemberCount)
	}
	if list[1].Key != "beta" {
		t.Errorf("list[1].Key = %q, want beta", list[1].Key)
	}
	if list[1].MemberCount != 0 {
		t.Errorf("beta MemberCount = %d, want 0", list[1].MemberCount)
	}

	// Cross-env: e2 must see its own (empty) list.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	otherList, err := repo.List(ctx, other)
	if err != nil {
		t.Fatalf("List e2: %v", err)
	}
	if len(otherList) != 0 {
		t.Fatalf("e2 must not see e1 topics, got %d", len(otherList))
	}
}

func TestTopicRepo_Delete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Topics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Create(ctx, scope, "to-del", "To Delete"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.AddSubscriber(ctx, scope, "to-del", "sub-1"); err != nil {
		t.Fatalf("add sub: %v", err)
	}

	if err := repo.Delete(ctx, scope, "to-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Topic is gone.
	if _, err := repo.GetByKey(ctx, scope, "to-del"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}

	// Membership is gone.
	ids, err := repo.ListSubscriberIDs(ctx, scope, "to-del")
	if err != nil {
		t.Fatalf("ListSubscriberIDs after delete: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty membership after delete, got %v", ids)
	}

	// Deleting again returns ErrNotFound.
	if err := repo.Delete(ctx, scope, "to-del"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestTopicRepo_RemoveSubscriber(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Topics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Create(ctx, scope, "team", "Team"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.AddSubscriber(ctx, scope, "team", "sub-a"); err != nil {
		t.Fatalf("add sub-a: %v", err)
	}
	if err := repo.AddSubscriber(ctx, scope, "team", "sub-b"); err != nil {
		t.Fatalf("add sub-b: %v", err)
	}

	if err := repo.RemoveSubscriber(ctx, scope, "team", "sub-a"); err != nil {
		t.Fatalf("RemoveSubscriber: %v", err)
	}

	ids, err := repo.ListSubscriberIDs(ctx, scope, "team")
	if err != nil {
		t.Fatalf("ListSubscriberIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sub-b" {
		t.Errorf("after remove: expected [sub-b], got %v", ids)
	}

	// Removing from non-existent topic returns ErrNotFound.
	if err := repo.RemoveSubscriber(ctx, scope, "ghost", "sub-b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveSubscriber ghost = %v, want ErrNotFound", err)
	}
}
