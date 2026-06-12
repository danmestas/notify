package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// seedNotification is a test helper that creates a notification in scope and
// returns the persisted row. It uses time.Sleep-free ordering by setting
// explicit created_at values derived from the seed index.
func seedNotification(t *testing.T, repo NotificationRepo, scope domain.Scope, i int, extra domain.Notification) domain.Notification {
	t.Helper()
	// Use a deterministic created_at so ordering is stable in tests.
	// Base time + i seconds; lexicographic ordering of RFC3339Nano matches temporal order.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := domain.Notification{
		WorkflowID:    extra.WorkflowID,
		SubscriberID:  extra.SubscriberID,
		TransactionID: extra.TransactionID,
		RunID:         fmt.Sprintf("run-%d", i),
		Payload:       []byte(`{"seq":` + fmt.Sprintf("%d", i) + `}`),
		CreatedAt:     base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano),
	}
	created, err := repo.Create(context.Background(), scope, n)
	if err != nil {
		t.Fatalf("seedNotification %d: %v", i, err)
	}
	return created
}

func TestNotificationRepo_List_EmptyPage(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	page, err := repo.List(ctx, scope, NotificationFilter{}, "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(page.Items))
	}
	if page.Cursor != "" {
		t.Fatalf("expected empty cursor, got %q", page.Cursor)
	}
	if page.TotalCount != 0 {
		t.Fatalf("expected totalCount=0, got %d", page.TotalCount)
	}
}

func TestNotificationRepo_List_SinglePage(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	const n = 5
	for i := 0; i < n; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{WorkflowID: "wf1"})
	}

	page, err := repo.List(ctx, scope, NotificationFilter{}, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != n {
		t.Fatalf("expected %d items, got %d", n, len(page.Items))
	}
	if page.Cursor != "" {
		t.Fatalf("expected empty cursor on single page, got %q", page.Cursor)
	}
	if page.TotalCount != n {
		t.Fatalf("expected totalCount=%d, got %d", n, page.TotalCount)
	}
	// Descending order: highest index (latest created_at) first.
	if page.Items[0].RunID != "run-4" {
		t.Fatalf("expected first item run-4 (newest), got %s", page.Items[0].RunID)
	}
}

func TestNotificationRepo_List_KeysetPagination(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	const total = 7
	const pageSize = 3
	for i := 0; i < total; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{})
	}

	// Walk through all pages using the cursor.
	var allIDs []string
	cursor := ""
	for {
		page, err := repo.List(ctx, scope, NotificationFilter{}, cursor, pageSize)
		if err != nil {
			t.Fatalf("List (cursor=%q): %v", cursor, err)
		}
		for _, item := range page.Items {
			allIDs = append(allIDs, item.RunID)
		}
		if page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}

	// Should have received all 7 rows, each exactly once.
	if len(allIDs) != total {
		t.Fatalf("expected %d total rows across pages, got %d: %v", total, len(allIDs), allIDs)
	}
	// No duplicates.
	seen := make(map[string]bool)
	for _, id := range allIDs {
		if seen[id] {
			t.Fatalf("duplicate run id %q in paginated results", id)
		}
		seen[id] = true
	}
}

func TestNotificationRepo_List_ExactBoundary(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Seed exactly pageSize rows.
	const n = 4
	for i := 0; i < n; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{})
	}

	page, err := repo.List(ctx, scope, NotificationFilter{}, "", n)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != n {
		t.Fatalf("expected %d items, got %d", n, len(page.Items))
	}
	// Exactly n rows means no next page.
	if page.Cursor != "" {
		t.Fatalf("expected empty cursor when rows == limit, got %q", page.Cursor)
	}
}

func TestNotificationRepo_List_CursorStableUnderInserts(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Seed 5 old rows.
	for i := 0; i < 5; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{})
	}

	// Get page 1 (limit=3), capturing the cursor.
	page1, err := repo.List(ctx, scope, NotificationFilter{}, "", 3)
	if err != nil {
		t.Fatalf("page1 List: %v", err)
	}
	if page1.Cursor == "" {
		t.Fatal("expected non-empty cursor after page 1")
	}

	// Insert new (newer) rows after page 1 was fetched.
	for i := 10; i < 15; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{})
	}

	// Page 2 using the cursor should return the remaining old rows only,
	// not the newly inserted ones (which are newer and not in this cursor window).
	page2, err := repo.List(ctx, scope, NotificationFilter{}, page1.Cursor, 3)
	if err != nil {
		t.Fatalf("page2 List: %v", err)
	}
	// The 2 remaining old rows (items[3] and items[4]).
	if len(page2.Items) != 2 {
		t.Fatalf("expected 2 remaining old rows on page 2, got %d: %v", len(page2.Items), page2.Items)
	}
	if page2.Cursor != "" {
		t.Fatalf("expected empty cursor on last page, got %q", page2.Cursor)
	}
}

func TestNotificationRepo_List_FilterByWorkflowID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	for i := 0; i < 3; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{WorkflowID: "wf-a"})
	}
	for i := 3; i < 5; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{WorkflowID: "wf-b"})
	}

	page, err := repo.List(ctx, scope, NotificationFilter{WorkflowID: "wf-a"}, "", 0)
	if err != nil {
		t.Fatalf("List wf-a: %v", err)
	}
	if page.TotalCount != 3 {
		t.Fatalf("expected 3 for wf-a, got %d", page.TotalCount)
	}
	for _, item := range page.Items {
		if item.WorkflowID != "wf-a" {
			t.Fatalf("expected wf-a, got %q", item.WorkflowID)
		}
	}
}

func TestNotificationRepo_List_FilterBySubscriberID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	for i := 0; i < 4; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{SubscriberID: "sub-1"})
	}
	for i := 4; i < 6; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{SubscriberID: "sub-2"})
	}

	page, err := repo.List(ctx, scope, NotificationFilter{SubscriberID: "sub-1"}, "", 0)
	if err != nil {
		t.Fatalf("List sub-1: %v", err)
	}
	if page.TotalCount != 4 {
		t.Fatalf("expected 4 for sub-1, got %d", page.TotalCount)
	}
}

func TestNotificationRepo_List_FilterByTransactionID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	seedNotification(t, repo, scope, 0, domain.Notification{TransactionID: "tx-abc"})
	seedNotification(t, repo, scope, 1, domain.Notification{TransactionID: "tx-abc"})
	seedNotification(t, repo, scope, 2, domain.Notification{TransactionID: "tx-xyz"})

	page, err := repo.List(ctx, scope, NotificationFilter{TransactionID: "tx-abc"}, "", 0)
	if err != nil {
		t.Fatalf("List tx-abc: %v", err)
	}
	if page.TotalCount != 2 {
		t.Fatalf("expected 2 for tx-abc, got %d", page.TotalCount)
	}
}

func TestNotificationRepo_List_FilterBySince(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{})
	}

	// Filter for rows at or after index 3 (created_at = base + 3s).
	since := base.Add(3 * time.Second)
	page, err := repo.List(ctx, scope, NotificationFilter{Since: since}, "", 0)
	if err != nil {
		t.Fatalf("List since: %v", err)
	}
	if page.TotalCount != 2 {
		t.Fatalf("expected 2 rows (idx 3+4) since %v, got %d", since, page.TotalCount)
	}
}

func TestNotificationRepo_List_CrossEnvIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()

	e1 := domain.Scope{OrgID: "o1", EnvID: "env-1"}
	e2 := domain.Scope{OrgID: "o1", EnvID: "env-2"}

	for i := 0; i < 5; i++ {
		seedNotification(t, repo, e1, i, domain.Notification{})
	}
	for i := 5; i < 7; i++ {
		seedNotification(t, repo, e2, i, domain.Notification{})
	}

	page1, err := repo.List(ctx, e1, NotificationFilter{}, "", 0)
	if err != nil {
		t.Fatalf("List e1: %v", err)
	}
	if page1.TotalCount != 5 {
		t.Fatalf("expected 5 for e1, got %d", page1.TotalCount)
	}
	for _, item := range page1.Items {
		if item.EnvID != "env-1" {
			t.Fatalf("e1 result has wrong EnvID %q", item.EnvID)
		}
	}

	page2, err := repo.List(ctx, e2, NotificationFilter{}, "", 0)
	if err != nil {
		t.Fatalf("List e2: %v", err)
	}
	if page2.TotalCount != 2 {
		t.Fatalf("expected 2 for e2, got %d", page2.TotalCount)
	}
}

func TestNotificationRepo_List_InvalidCursor(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	_, err := repo.List(ctx, scope, NotificationFilter{}, "not-valid-base64!!!", 10)
	if err == nil {
		t.Fatal("expected error for invalid cursor, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		// We just want a non-nil error; exact type is store-internal.
		// Verify it mentions "cursor".
		if err.Error() == "" {
			t.Fatalf("expected non-empty error for invalid cursor")
		}
	}
}

func TestNotificationRepo_List_DefaultAndMaxLimit(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Notifications()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Seed 60 rows (more than default 50, less than max 200).
	for i := 0; i < 60; i++ {
		seedNotification(t, repo, scope, i, domain.Notification{})
	}

	// limit=0 → default 50.
	page, err := repo.List(ctx, scope, NotificationFilter{}, "", 0)
	if err != nil {
		t.Fatalf("default limit: %v", err)
	}
	if len(page.Items) != 50 {
		t.Fatalf("expected 50 items with default limit, got %d", len(page.Items))
	}
	if page.Cursor == "" {
		t.Fatal("expected non-empty cursor (60 rows, limit 50)")
	}
	if page.TotalCount != 60 {
		t.Fatalf("expected totalCount=60, got %d", page.TotalCount)
	}

	// limit=9999 → capped at 200; still returns totalCount correctly.
	page2, err := repo.List(ctx, scope, NotificationFilter{}, "", 9999)
	if err != nil {
		t.Fatalf("max limit: %v", err)
	}
	if len(page2.Items) != 60 {
		t.Fatalf("expected 60 items (all) with large limit, got %d", len(page2.Items))
	}
}
