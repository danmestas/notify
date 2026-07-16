package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0006_AppliesAnalytics(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 30 {
		t.Fatalf("expected 30 recorded migrations, got %d", n)
	}

	var name string
	err := s.reader.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='analytics_deliveries'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("analytics_deliveries table missing: %v", err)
	}
}

func TestAnalyticsRepo_InsertAndCount(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	rows := []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "throttled"},
		{Channel: "email", Status: "sent"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	// Row under a different environment must not leak into scope A's counts.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if err := repo.Insert(ctx, other, domain.AnalyticsDelivery{Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("insert isolation row: %v", err)
	}

	got, err := repo.CountByChannelStatus(ctx, scope, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	want := map[string]map[string]int{
		"in_app": {"sent": 2, "throttled": 1},
		"email":  {"sent": 1},
	}
	if len(got) != len(want) {
		t.Fatalf("channel count mismatch: got %v want %v", got, want)
	}
	for channel, statuses := range want {
		for status, n := range statuses {
			if got[channel][status] != n {
				t.Fatalf("count[%s][%s]: got %d want %d (full %v)", channel, status, got[channel][status], n, got)
			}
		}
		if len(got[channel]) != len(statuses) {
			t.Fatalf("status count mismatch for %s: got %v want %v", channel, got[channel], statuses)
		}
	}
}

func TestAnalyticsRepo_DeliveriesByDay(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Explicit CreatedAt so day-bucketing is deterministic (two UTC days).
	rows := []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent", CreatedAt: "2026-06-08T10:00:00Z"},
		{Channel: "in_app", Status: "sent", CreatedAt: "2026-06-08T23:59:59Z"},
		{Channel: "in_app", Status: "sent", CreatedAt: "2026-06-09T00:00:01Z"},
		{Channel: "email", Status: "sent", CreatedAt: "2026-06-09T09:00:00Z"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	// A row under a different environment must not leak into scope's series.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if err := repo.Insert(ctx, other, domain.AnalyticsDelivery{Channel: "in_app", Status: "sent", CreatedAt: "2026-06-09T09:00:00Z"}); err != nil {
		t.Fatalf("insert isolation row: %v", err)
	}

	got, err := repo.DeliveriesByDay(ctx, scope, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("DeliveriesByDay: %v", err)
	}
	want := []domain.DeliveryDayCount{
		{Day: "2026-06-08", Channel: "in_app", Status: "sent", Count: 2},
		{Day: "2026-06-09", Channel: "email", Status: "sent", Count: 1},
		{Day: "2026-06-09", Channel: "in_app", Status: "sent", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeliveriesByDay mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestAnalyticsRepo_CountByChannelStatus_WorkflowFilter verifies that the
// workflow_id filter restricts results to rows for the specified workflow only,
// and that rows from other workflows do not appear (GAP-AF-06).
func TestAnalyticsRepo_CountByChannelStatus_WorkflowFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	rows := []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-a"},
		{Channel: "in_app", Status: "failed", WorkflowID: "wf-a"},
		{Channel: "sms", Status: "sent", WorkflowID: "wf-b"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	got, err := repo.CountByChannelStatus(ctx, scope, AnalyticsFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("CountByChannelStatus(wf-a): %v", err)
	}
	want := map[string]map[string]int{
		"email":  {"sent": 2},
		"in_app": {"failed": 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CountByChannelStatus(wf-a) mismatch:\n got=%v\nwant=%v", got, want)
	}

	// wf-b has only sms/sent.
	gotB, err := repo.CountByChannelStatus(ctx, scope, AnalyticsFilter{WorkflowID: "wf-b"})
	if err != nil {
		t.Fatalf("CountByChannelStatus(wf-b): %v", err)
	}
	wantB := map[string]map[string]int{"sms": {"sent": 1}}
	if !reflect.DeepEqual(gotB, wantB) {
		t.Fatalf("CountByChannelStatus(wf-b) mismatch:\n got=%v\nwant=%v", gotB, wantB)
	}
}

// TestAnalyticsRepo_DeliveriesByDay_WorkflowFilter proves workflow_id scoping on
// the time-series query (GAP-AF-06).
func TestAnalyticsRepo_DeliveriesByDay_WorkflowFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	rows := []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", WorkflowID: "wf-a", CreatedAt: "2026-06-09T10:00:00Z"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-a", CreatedAt: "2026-06-09T11:00:00Z"},
		{Channel: "in_app", Status: "sent", WorkflowID: "wf-b", CreatedAt: "2026-06-09T12:00:00Z"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	got, err := repo.DeliveriesByDay(ctx, scope, AnalyticsFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("DeliveriesByDay(wf-a): %v", err)
	}
	want := []domain.DeliveryDayCount{
		{Day: "2026-06-09", Channel: "email", Status: "sent", Count: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeliveriesByDay(wf-a) mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestAnalyticsRepo_RunsByDay proves RunsByDay counts DISTINCT run_ids per
// day, not delivery rows — the store-layer regression guard for QA finding B.
// A single run fanned out to 2 channels (in_app + stream) contributes 2 rows
// but must count as 1 run.
func TestAnalyticsRepo_RunsByDay(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	rows := []domain.AnalyticsDelivery{
		// run-1 fans out to in_app + stream: 2 rows, 1 run.
		{Channel: "in_app", Status: "sent", RunID: "run-1", CreatedAt: "2026-06-08T10:00:00Z"},
		{Channel: "stream", Status: "sent", RunID: "run-1", CreatedAt: "2026-06-08T10:00:01Z"},
		// run-2: a second, distinct run the same day.
		{Channel: "email", Status: "sent", RunID: "run-2", CreatedAt: "2026-06-08T11:00:00Z"},
		// run-3: a run on the next day.
		{Channel: "email", Status: "sent", RunID: "run-3", CreatedAt: "2026-06-09T09:00:00Z"},
		// A row with no run_id (pre-M2/legacy data) must not be counted as a run.
		{Channel: "email", Status: "sent", CreatedAt: "2026-06-09T09:30:00Z"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	// A row under a different environment must not leak into scope's series.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if err := repo.Insert(ctx, other, domain.AnalyticsDelivery{Channel: "email", Status: "sent", RunID: "run-other", CreatedAt: "2026-06-08T10:00:00Z"}); err != nil {
		t.Fatalf("insert isolation row: %v", err)
	}

	got, err := repo.RunsByDay(ctx, scope, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("RunsByDay: %v", err)
	}
	want := []domain.RunDayCount{
		{Day: "2026-06-08", Count: 2}, // run-1 (fanned out to 2 channels) + run-2
		{Day: "2026-06-09", Count: 1}, // run-3 only; the no-run_id row is excluded
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunsByDay mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestAnalyticsRepo_RunsByDay_WorkflowFilter proves workflow_id scoping on
// the runs-per-day query (M2, mirrors DeliveriesByDay_WorkflowFilter).
func TestAnalyticsRepo_RunsByDay_WorkflowFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	rows := []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent", WorkflowID: "wf-a", RunID: "run-a1", CreatedAt: "2026-06-09T10:00:00Z"},
		{Channel: "stream", Status: "sent", WorkflowID: "wf-a", RunID: "run-a1", CreatedAt: "2026-06-09T10:00:01Z"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-a", RunID: "run-a2", CreatedAt: "2026-06-09T11:00:00Z"},
		{Channel: "email", Status: "sent", WorkflowID: "wf-b", RunID: "run-b1", CreatedAt: "2026-06-09T12:00:00Z"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	got, err := repo.RunsByDay(ctx, scope, AnalyticsFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("RunsByDay(wf-a): %v", err)
	}
	want := []domain.RunDayCount{
		{Day: "2026-06-09", Count: 2}, // run-a1 (2 channels) + run-a2; wf-b's run-b1 excluded
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunsByDay(wf-a) mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestAnalyticsRepo_CountActiveSubscribers verifies the distinct-subscriber
// count, since-window, and cross-env isolation (GAP-AF-05).
func TestAnalyticsRepo_CountActiveSubscribers(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	base := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	rows := []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", SubscriberID: "sub-1", CreatedAt: base.Format(time.RFC3339Nano)},
		{Channel: "in_app", Status: "sent", SubscriberID: "sub-1", CreatedAt: base.Add(time.Hour).Format(time.RFC3339Nano)},
		{Channel: "email", Status: "sent", SubscriberID: "sub-2", CreatedAt: base.Add(2 * time.Hour).Format(time.RFC3339Nano)},
		// Old row that falls outside the since window.
		{Channel: "email", Status: "sent", SubscriberID: "sub-3", CreatedAt: base.Add(-48 * time.Hour).Format(time.RFC3339Nano)},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	// Isolation: row in a different env must not count.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	if err := repo.Insert(ctx, other, domain.AnalyticsDelivery{
		Channel: "email", Status: "sent", SubscriberID: "sub-4",
		CreatedAt: base.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("insert other-scope row: %v", err)
	}

	// Since base: only sub-1 and sub-2 are within the window.
	n, err := repo.CountActiveSubscribers(ctx, scope, base, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("CountActiveSubscribers: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountActiveSubscribers: got %d, want 2", n)
	}

	// Since zero: all three subscribers in scope (sub-1, sub-2, sub-3).
	nAll, err := repo.CountActiveSubscribers(ctx, scope, time.Time{}, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("CountActiveSubscribers(zero since): %v", err)
	}
	if nAll != 3 {
		t.Fatalf("CountActiveSubscribers(zero): got %d, want 3", nAll)
	}
}

// TestAnalyticsRepo_CountActiveSubscribers_WorkflowFilter proves the
// AnalyticsFilter.WorkflowID predicate scopes the distinct-subscriber count to
// only that workflow's rows (M1 fix — the KPI tile previously ignored the
// workflow filter despite a doc comment claiming otherwise, GAP-AF-05/06).
func TestAnalyticsRepo_CountActiveSubscribers_WorkflowFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	rows := []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", SubscriberID: "sub-1", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", SubscriberID: "sub-2", WorkflowID: "wf-a"},
		// sub-3 only ever appears on wf-b.
		{Channel: "email", Status: "sent", SubscriberID: "sub-3", WorkflowID: "wf-b"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	n, err := repo.CountActiveSubscribers(ctx, scope, time.Time{}, AnalyticsFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("CountActiveSubscribers(wf-a): %v", err)
	}
	if n != 2 {
		t.Fatalf("CountActiveSubscribers(wf-a): got %d, want 2", n)
	}

	nB, err := repo.CountActiveSubscribers(ctx, scope, time.Time{}, AnalyticsFilter{WorkflowID: "wf-b"})
	if err != nil {
		t.Fatalf("CountActiveSubscribers(wf-b): %v", err)
	}
	if nB != 1 {
		t.Fatalf("CountActiveSubscribers(wf-b): got %d, want 1", nB)
	}
}

// TestAnalyticsRepo_AverageMessagesPerSubscriber verifies the average delivery
// count per subscriber, since-window, and the zero-row case (GAP-AF-05).
func TestAnalyticsRepo_AverageMessagesPerSubscriber(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	base := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

	// sub-1 gets 2 rows, sub-2 gets 1 row → average = 1.5.
	rows := []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", SubscriberID: "sub-1", CreatedAt: base.Format(time.RFC3339Nano)},
		{Channel: "in_app", Status: "sent", SubscriberID: "sub-1", CreatedAt: base.Add(time.Hour).Format(time.RFC3339Nano)},
		{Channel: "email", Status: "sent", SubscriberID: "sub-2", CreatedAt: base.Add(2 * time.Hour).Format(time.RFC3339Nano)},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	avg, err := repo.AverageMessagesPerSubscriber(ctx, scope, base, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("AverageMessagesPerSubscriber: %v", err)
	}
	// 3 rows / 2 subscribers = 1.5
	const want = 1.5
	if avg != want {
		t.Fatalf("AverageMessagesPerSubscriber: got %v, want %v", avg, want)
	}

	// Empty store → zero.
	emptyScope := domain.Scope{OrgID: "o1", EnvID: "empty"}
	avgEmpty, err := repo.AverageMessagesPerSubscriber(ctx, emptyScope, time.Time{}, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("AverageMessagesPerSubscriber(empty): %v", err)
	}
	if avgEmpty != 0 {
		t.Fatalf("AverageMessagesPerSubscriber(empty): got %v, want 0", avgEmpty)
	}
}

// TestAnalyticsRepo_AverageMessagesPerSubscriber_WorkflowFilter proves the
// AnalyticsFilter.WorkflowID predicate scopes the average to only that
// workflow's rows (M1 fix).
func TestAnalyticsRepo_AverageMessagesPerSubscriber_WorkflowFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// wf-a: sub-1 gets 2 rows, sub-2 gets 1 row → average = 1.5.
	// wf-b: sub-3 gets 1 row → average = 1.0.
	rows := []domain.AnalyticsDelivery{
		{Channel: "email", Status: "sent", SubscriberID: "sub-1", WorkflowID: "wf-a"},
		{Channel: "in_app", Status: "sent", SubscriberID: "sub-1", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", SubscriberID: "sub-2", WorkflowID: "wf-a"},
		{Channel: "email", Status: "sent", SubscriberID: "sub-3", WorkflowID: "wf-b"},
	}
	for i, r := range rows {
		if err := repo.Insert(ctx, scope, r); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	avgA, err := repo.AverageMessagesPerSubscriber(ctx, scope, time.Time{}, AnalyticsFilter{WorkflowID: "wf-a"})
	if err != nil {
		t.Fatalf("AverageMessagesPerSubscriber(wf-a): %v", err)
	}
	if avgA != 1.5 {
		t.Fatalf("AverageMessagesPerSubscriber(wf-a): got %v, want 1.5", avgA)
	}

	avgB, err := repo.AverageMessagesPerSubscriber(ctx, scope, time.Time{}, AnalyticsFilter{WorkflowID: "wf-b"})
	if err != nil {
		t.Fatalf("AverageMessagesPerSubscriber(wf-b): %v", err)
	}
	if avgB != 1.0 {
		t.Fatalf("AverageMessagesPerSubscriber(wf-b): got %v, want 1.0", avgB)
	}
}

// TestAnalyticsRepo_CountActiveSubscribers_ScopeIsolation proves that rows
// from a different org/env are not included in counts (GAP-AF-05).
func TestAnalyticsRepo_CountActiveSubscribers_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Analytics()

	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o1", EnvID: "e2"}
	base := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

	_ = repo.Insert(ctx, scopeA, domain.AnalyticsDelivery{SubscriberID: "sub-a", Channel: "email", Status: "sent", CreatedAt: base.Format(time.RFC3339Nano)})
	_ = repo.Insert(ctx, scopeB, domain.AnalyticsDelivery{SubscriberID: "sub-b", Channel: "email", Status: "sent", CreatedAt: base.Format(time.RFC3339Nano)})

	n, err := repo.CountActiveSubscribers(ctx, scopeA, time.Time{}, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("CountActiveSubscribers: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1, got %d (cross-scope leak?)", n)
	}
}
