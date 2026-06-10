//go:build duckdb

package analyticsduck

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// compile-time proof that the DuckDB backend is a drop-in store.AnalyticsRepo.
var _ store.AnalyticsRepo = (*Repo)(nil)

// TestRepo_InsertAndCount mirrors store.TestAnalyticsRepo_InsertAndCount against
// the DuckDB backend: same rows, same cross-environment isolation row, same
// expected aggregation — proving the two AnalyticsRepo implementations agree.
func TestRepo_InsertAndCount(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, "") // in-memory DuckDB
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = repo.Close() }()

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

	// A row under a different environment must not leak into scope's counts.
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

// TestRepo_MintsIDAndTimestamp verifies Insert fills id/created_at when empty
// (so the projector can append bare observations) and persists them.
func TestRepo_MintsIDAndTimestamp(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = repo.Close() }()

	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if err := repo.Insert(ctx, scope, domain.AnalyticsDelivery{Channel: "sms", Status: "sent"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var id, createdAt string
	if err := repo.db.QueryRowContext(
		ctx,
		`SELECT id, created_at FROM analytics_deliveries WHERE channel = ?`, "sms",
	).Scan(&id, &createdAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if id == "" {
		t.Fatal("expected minted id, got empty")
	}
	if createdAt == "" {
		t.Fatal("expected minted created_at, got empty")
	}
}

// TestRepo_DeliveriesByDay mirrors store.TestAnalyticsRepo_DeliveriesByDay
// against DuckDB: identical rows + isolation, identical ordered day-series —
// proving the shared store.QueryDeliveriesByDay yields the same result on DuckDB.
func TestRepo_DeliveriesByDay(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = repo.Close() }()

	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
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
		t.Fatalf("DuckDB DeliveriesByDay mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestRepo_FilePersistence proves DuckDB access via a real on-disk database:
// rows written through one handle survive close + reopen of the same file.
func TestRepo_FilePersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.duckdb")
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	repo, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := repo.Insert(ctx, scope, domain.AnalyticsDelivery{Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open 2 (reopen file): %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CountByChannelStatus(ctx, scope)
	if err != nil {
		t.Fatalf("count after reopen: %v", err)
	}
	if got["in_app"]["sent"] != 1 {
		t.Fatalf("persistence: in_app.sent = %d after reopen, want 1 (full %v)", got["in_app"]["sent"], got)
	}
}
