package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestNotificationRepo_GetByRunID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID:    "wf1",
		SubscriberID:  "sub1",
		TransactionID: "tx1",
		RunID:         "r1",
		Payload:       []byte(`{"name":"Dan"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByRunID(ctx, "r1")
	if err != nil {
		t.Fatalf("GetByRunID: %v", err)
	}
	if got.ID != created.ID || got.OrgID != "o1" || got.EnvID != "e1" ||
		got.WorkflowID != "wf1" || got.SubscriberID != "sub1" {
		t.Fatalf("unexpected notification: %+v", got)
	}
	if string(got.Payload) != `{"name":"Dan"}` {
		t.Fatalf("payload mismatch: %s", got.Payload)
	}

	if _, err := repo.GetByRunID(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown run id, got %v", err)
	}
}

func TestNotificationRepo_DigestKey_RoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID:   "wf1",
		SubscriberID: "sub1",
		RunID:        "rd1",
		DigestKey:    "env:sub:wf:v",
	}); err != nil {
		t.Fatalf("create digest: %v", err)
	}
	got, err := repo.GetByRunID(ctx, "rd1")
	if err != nil {
		t.Fatalf("GetByRunID: %v", err)
	}
	if got.DigestKey != "env:sub:wf:v" {
		t.Fatalf("digest key mismatch: got %q", got.DigestKey)
	}

	if _, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID:   "wf1",
		SubscriberID: "sub1",
		RunID:        "rd2",
	}); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	plain, err := repo.GetByRunID(ctx, "rd2")
	if err != nil {
		t.Fatalf("GetByRunID plain: %v", err)
	}
	if plain.DigestKey != "" {
		t.Fatalf("expected empty digest key, got %q", plain.DigestKey)
	}
}

// TestNotificationRepo_TriggerLifecycle covers the rows the trigger writes
// ahead of a run (#66): found again by (workflow, subscriber, transaction),
// given a run id exactly once, and marked failed only while it has none.
func TestNotificationRepo_TriggerLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	if _, err := repo.GetByTrigger(ctx, scope, "wf1", "sub1", "tx1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByTrigger before create: err = %v, want ErrNotFound", err)
	}
	n, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID: "wf1", SubscriberID: "sub1", TransactionID: "tx1", Status: domain.NotificationPending,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetByTrigger(ctx, scope, "wf1", "sub1", "tx1")
	if err != nil || got.ID != n.ID || got.Status != domain.NotificationPending {
		t.Fatalf("GetByTrigger = %+v, %v; want %s pending", got, err, n.ID)
	}
	for _, miss := range [][3]string{{"wf2", "sub1", "tx1"}, {"wf1", "sub2", "tx1"}, {"wf1", "sub1", "tx2"}} {
		if _, err := repo.GetByTrigger(ctx, scope, miss[0], miss[1], miss[2]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetByTrigger(%v): err = %v, want ErrNotFound", miss, err)
		}
	}
	if _, err := repo.GetByTrigger(ctx, other, "wf1", "sub1", "tx1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByTrigger in another env: err = %v, want ErrNotFound", err)
	}

	if err := repo.MarkFailed(ctx, scope, n.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if got, _ := repo.Get(ctx, scope, n.ID); got.Status != domain.NotificationFailed {
		t.Fatalf("status after MarkFailed = %q, want failed", got.Status)
	}
	if ok, err := repo.AttachRun(ctx, other, n.ID, "r1"); err != nil || ok {
		t.Fatalf("AttachRun in another env = %v, %v; want false", ok, err)
	}
	if ok, err := repo.AttachRun(ctx, scope, n.ID, "r1"); err != nil || !ok {
		t.Fatalf("AttachRun = %v, %v; want true", ok, err)
	}
	if ok, err := repo.AttachRun(ctx, scope, n.ID, "r2"); err != nil || ok {
		t.Fatalf("second AttachRun = %v, %v; want false", ok, err)
	}
	if err := repo.MarkFailed(ctx, scope, n.ID); err != nil {
		t.Fatalf("MarkFailed after run: %v", err)
	}
	got, err = repo.Get(ctx, scope, n.ID)
	if err != nil || got.RunID != "r1" || got.Status != domain.NotificationStarted {
		t.Fatalf("row = %+v, %v; want r1 started", got, err)
	}

	legacy, err := repo.Create(ctx, scope, domain.Notification{WorkflowID: "wf1", RunID: "r9"})
	if err != nil || legacy.Status != domain.NotificationStarted {
		t.Fatalf("create without status = %+v, %v; want started", legacy, err)
	}
}
