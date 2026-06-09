package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMigrate_0006_AppliesAnalytics(t *testing.T) {
	s := openTemp(t)

	if n := migrationCount(t, s); n != 6 {
		t.Fatalf("expected 6 recorded migrations, got %d", n)
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
