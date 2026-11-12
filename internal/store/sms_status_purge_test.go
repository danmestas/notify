package store

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

func pendingCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.reader.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sms_status_pending`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestPurgeExpiredPendingStatuses: only buffered statuses past their life go,
// without any new status arriving to trigger it.
func TestPurgeExpiredPendingStatuses(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	for _, sid := range []string{"old", "fresh"} {
		if _, err := s.Messages().ApplyStatusCallback(ctx, scope, "int", sid, "delivered", "", ""); err != nil {
			t.Fatalf("buffer %s: %v", sid, err)
		}
	}
	if _, err := s.writer.ExecContext(ctx, `UPDATE sms_status_pending SET received_at = ? WHERE provider_message_id = 'old'`,
		timestamp.Format(time.Now().Add(-pendingStatusLife-time.Minute))); err != nil {
		t.Fatalf("age: %v", err)
	}
	n, err := s.PurgeExpiredPendingStatuses(ctx)
	if err != nil || n != 1 {
		t.Fatalf("PurgeExpiredPendingStatuses = %d, %v; want 1", n, err)
	}
	if got := pendingCount(t, s); got != 1 {
		t.Fatalf("%d rows left, want the fresh one", got)
	}
	if n, err := s.PurgeExpiredPendingStatuses(ctx); err != nil || n != 0 {
		t.Fatalf("second purge = %d, %v; want 0", n, err)
	}
}

// TestApplyStatusCallback_BufferingDoesNotPurge: purging is the periodic
// loop's job, so buffering a status leaves other expired rows alone.
func TestApplyStatusCallback_BufferingDoesNotPurge(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if _, err := s.Messages().ApplyStatusCallback(ctx, scope, "int", "old", "delivered", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.ExecContext(ctx, `UPDATE sms_status_pending SET received_at = ?`,
		timestamp.Format(time.Now().Add(-2*pendingStatusLife))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Messages().ApplyStatusCallback(ctx, scope, "int", "new", "delivered", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := pendingCount(t, s); got != 2 {
		t.Fatalf("%d rows, want 2: buffering should not purge", got)
	}
}

// TestSMSOptOuts_ReleaseHelpReply: a released claim can be taken again inside
// the window.
func TestSMSOptOuts_ReleaseHelpReply(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.SMSOptOuts()
	if ok, err := repo.ClaimHelpReply(ctx, scope, "int", "+15551112222", 10*time.Minute); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, _ := repo.ClaimHelpReply(ctx, scope, "int", "+15551112222", 10*time.Minute); ok {
		t.Fatal("second claim granted inside the window")
	}
	if err := repo.ReleaseHelpReply(ctx, scope, "int", "+15551112222"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := repo.ClaimHelpReply(ctx, scope, "int", "+15551112222", 10*time.Minute); err != nil || !ok {
		t.Fatalf("claim after release = %v, %v", ok, err)
	}
}
