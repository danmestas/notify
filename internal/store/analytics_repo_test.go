package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0006_AppliesAnalytics(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 13 {
		t.Fatalf("expected 13 recorded migrations, got %d", n)
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

	got, err := repo.CountByChannelStatus(ctx, scope)
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

	got, err := repo.DeliveriesByDay(ctx, scope)
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
