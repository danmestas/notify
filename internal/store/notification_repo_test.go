package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
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

// TestNotificationRepo_FailStalePendingAndStatusFilter: the sweep fails only
// pending rows with no run that are older than the cutoff, and List filters on
// status while listing every status by default.
func TestNotificationRepo_FailStalePendingAndStatusFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	now := time.Now()
	old := timestamp.Format(now.Add(-time.Hour))
	rows := []domain.Notification{
		{ID: "stale", TransactionID: "tx", Status: domain.NotificationPending, CreatedAt: old},
		{ID: "fresh", TransactionID: "tx", Status: domain.NotificationPending, CreatedAt: timestamp.Format(now)},
		{ID: "has-run", TransactionID: "tx", Status: domain.NotificationPending, RunID: "r1", CreatedAt: old},
		{ID: "started", TransactionID: "tx", Status: domain.NotificationStarted, RunID: "r2", CreatedAt: old},
	}
	for _, n := range rows {
		if _, err := repo.Create(ctx, scope, n); err != nil {
			t.Fatalf("create %s: %v", n.ID, err)
		}
	}

	n, err := repo.FailStalePending(ctx, now.Add(-15*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("FailStalePending = %d, %v; want 1", n, err)
	}
	want := map[string]string{
		"stale":   domain.NotificationFailed,
		"fresh":   domain.NotificationPending,
		"has-run": domain.NotificationPending,
		"started": domain.NotificationStarted,
	}
	for id, status := range want {
		if got, _ := repo.Get(ctx, scope, id); got.Status != status {
			t.Fatalf("%s: status = %q, want %q", id, got.Status, status)
		}
	}

	all, err := repo.List(ctx, scope, NotificationFilter{TransactionID: "tx"}, "", 0)
	if err != nil || len(all.Items) != 4 {
		t.Fatalf("List all = %d rows, %v; want 4", len(all.Items), err)
	}
	pending, err := repo.List(ctx, scope, NotificationFilter{TransactionID: "tx", Status: domain.NotificationPending}, "", 0)
	if err != nil || len(pending.Items) != 2 || pending.TotalCount != 2 {
		t.Fatalf("List pending = %d rows (total %d), %v; want 2", len(pending.Items), pending.TotalCount, err)
	}
}

// TestNotificationRepo_MarkCancelled covers #144's store side: a cancelled row
// stays cancelled through every later write a trigger or a step could make
// (recording a run, failing a refused start, the stale-pending sweep), and
// MarkCancelled only touches a row still naming the run it was read with.
func TestNotificationRepo_MarkCancelled(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	old := time.Now().Add(-time.Hour)

	pending, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID: "wf1", SubscriberID: "sub1", TransactionID: "tx1",
		Status: domain.NotificationPending, CreatedAt: timestamp.Format(old),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A run recorded after the row was read makes the no-run cancel miss.
	if ok, err := repo.MarkCancelled(ctx, scope, pending.ID, "r-other"); err != nil || ok {
		t.Fatalf("MarkCancelled with the wrong run = %v, %v; want false", ok, err)
	}
	if ok, err := repo.MarkCancelled(ctx, scope, pending.ID, ""); err != nil || !ok {
		t.Fatalf("MarkCancelled = %v, %v; want true", ok, err)
	}
	if ok, err := repo.MarkCancelled(ctx, scope, pending.ID, ""); err != nil || !ok {
		t.Fatalf("second MarkCancelled = %v, %v; want true (idempotent)", ok, err)
	}
	if ok, err := repo.MarkCancelled(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, pending.ID, ""); err != nil || ok {
		t.Fatalf("MarkCancelled from another env = %v, %v; want false", ok, err)
	}

	if _, err := repo.FailStalePending(ctx, time.Now()); err != nil {
		t.Fatalf("FailStalePending: %v", err)
	}
	if err := repo.MarkFailed(ctx, scope, pending.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if _, err := repo.AttachRun(ctx, scope, pending.ID, "r1"); err != nil {
		t.Fatalf("AttachRun: %v", err)
	}
	got, err := repo.Get(ctx, scope, pending.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.NotificationCancelled || got.RunID != "r1" {
		t.Fatalf("row = status %q run %q, want cancelled with run r1 recorded", got.Status, got.RunID)
	}

	started, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID: "wf1", SubscriberID: "sub2", TransactionID: "tx1", RunID: "r2",
	})
	if err != nil {
		t.Fatalf("create started: %v", err)
	}
	if ok, err := repo.MarkCancelled(ctx, scope, started.ID, "r2"); err != nil || !ok {
		t.Fatalf("MarkCancelled started = %v, %v; want true", ok, err)
	}
	page, err := repo.List(ctx, scope, NotificationFilter{Status: domain.NotificationCancelled}, "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.TotalCount != 2 {
		t.Fatalf("cancelled rows = %d, want 2", page.TotalCount)
	}
}
