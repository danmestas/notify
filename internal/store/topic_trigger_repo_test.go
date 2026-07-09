package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0016_AppliesTopicTriggers(t *testing.T) {
	s := openTemp(t)
	if n := migrationCount(t, s); n != 27 {
		t.Fatalf("expected 26 recorded migrations, got %d", n)
	}
	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='topic_triggers'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("topic_triggers table missing: %v", err)
	}
}

func TestTopicTriggerRepo_Record(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.TopicTriggers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// First insert succeeds.
	if err := repo.Record(ctx, scope, "txn-1", "topic-key-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Duplicate is silently ignored (idempotent).
	if err := repo.Record(ctx, scope, "txn-1", "topic-key-1"); err != nil {
		t.Fatalf("Record duplicate: %v", err)
	}

	// Different transaction in same scope.
	if err := repo.Record(ctx, scope, "txn-2", "topic-key-1"); err != nil {
		t.Fatalf("Record second txn: %v", err)
	}

	// Confirm two distinct rows exist.
	var n int
	if err := s.reader.QueryRow(
		`SELECT COUNT(*) FROM topic_triggers WHERE environment_id='e1' AND topic_key='topic-key-1'`,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows, got %d", n)
	}
}

func TestTopicTriggerRepo_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.TopicTriggers()

	scopeA := domain.Scope{OrgID: "orgA", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "orgB", EnvID: "envB"}

	if err := repo.Record(ctx, scopeA, "txn-1", "k"); err != nil {
		t.Fatalf("Record scopeA: %v", err)
	}
	if err := repo.Record(ctx, scopeB, "txn-2", "k"); err != nil {
		t.Fatalf("Record scopeB: %v", err)
	}

	pageA, err := repo.ListByTopic(ctx, scopeA, "k", "", 25)
	if err != nil {
		t.Fatalf("ListByTopic scopeA: %v", err)
	}
	if len(pageA.Items) != 1 {
		t.Fatalf("scopeA: expected 1 item, got %d", len(pageA.Items))
	}

	pageB, err := repo.ListByTopic(ctx, scopeB, "k", "", 25)
	if err != nil {
		t.Fatalf("ListByTopic scopeB: %v", err)
	}
	if len(pageB.Items) != 1 {
		t.Fatalf("scopeB: expected 1 item, got %d", len(pageB.Items))
	}
}

func TestTopicTriggerRepo_ListByTopic_Pagination(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.TopicTriggers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Insert 5 records.
	for i := 1; i <= 5; i++ {
		txn := "txn-pag-" + string(rune('a'+i-1))
		if err := repo.Record(ctx, scope, txn, "pag-topic"); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	// First page of 2.
	p1, err := repo.ListByTopic(ctx, scope, "pag-topic", "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(p1.Items) != 2 {
		t.Fatalf("page1: expected 2 items, got %d", len(p1.Items))
	}
	if p1.NextCursor == "" {
		t.Fatal("page1: expected non-empty NextCursor")
	}

	// Second page using cursor.
	p2, err := repo.ListByTopic(ctx, scope, "pag-topic", p1.NextCursor, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(p2.Items) != 2 {
		t.Fatalf("page2: expected 2 items, got %d", len(p2.Items))
	}

	// Third page — last item.
	p3, err := repo.ListByTopic(ctx, scope, "pag-topic", p2.NextCursor, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(p3.Items) != 1 {
		t.Fatalf("page3: expected 1 item, got %d", len(p3.Items))
	}
	if p3.NextCursor != "" {
		t.Fatalf("page3: expected empty NextCursor, got %q", p3.NextCursor)
	}
}

func TestTopicTriggerRepo_ListByTopic_JoinAnalytics(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	topicRepo := s.TopicTriggers()
	analyticsRepo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Record a topic trigger.
	if err := topicRepo.Record(ctx, scope, "txn-joined", "my-topic"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Insert a matching analytics delivery.
	if err := analyticsRepo.Insert(ctx, scope, domain.AnalyticsDelivery{
		Channel:       "in_app",
		Status:        "sent",
		SubscriberID:  "sub-1",
		WorkflowID:    "wf-1",
		TransactionID: "txn-joined",
	}); err != nil {
		t.Fatalf("analytics Insert: %v", err)
	}

	page, err := topicRepo.ListByTopic(ctx, scope, "my-topic", "", 25)
	if err != nil {
		t.Fatalf("ListByTopic: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(page.Items))
	}
	item := page.Items[0]
	if item.TransactionID != "txn-joined" {
		t.Errorf("TransactionID = %q, want %q", item.TransactionID, "txn-joined")
	}
	if item.Channel != "in_app" {
		t.Errorf("Channel = %q, want %q", item.Channel, "in_app")
	}
	if item.Status != "sent" {
		t.Errorf("Status = %q, want %q", item.Status, "sent")
	}
	if item.SubscriberID != "sub-1" {
		t.Errorf("SubscriberID = %q, want %q", item.SubscriberID, "sub-1")
	}
}
