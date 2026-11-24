package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestSMSCaptured_KeepsNewestPerIntegration: past CapturedSMSKeep an
// integration drops its oldest texts and another integration's are untouched.
func TestSMSCaptured_KeepsNewestPerIntegration(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.SMSCaptured()

	seen := map[string]bool{}
	var lastPMID string
	for i := 0; i < CapturedSMSKeep+5; i++ {
		pmid, err := repo.Add(ctx, scope, "int-a", "+15551112222", "+15550100000", "msg")
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		if seen[pmid] {
			t.Fatalf("provider message id %q repeated", pmid)
		}
		seen[pmid] = true
		lastPMID = pmid
	}
	if _, err := repo.Add(ctx, scope, "int-b", "+15551112222", "+15550100000", "other"); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM sms_captured WHERE integration_id = 'int-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != CapturedSMSKeep {
		t.Fatalf("int-a rows = %d, want %d", n, CapturedSMSKeep)
	}
	rows, err := repo.List(ctx, scope, "int-a", 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("list = %d rows, %v", len(rows), err)
	}
	last := lastPMID
	if rows[0].ProviderMessageID != last {
		t.Errorf("newest = %q, want the last added %q", rows[0].ProviderMessageID, last)
	}
	if rows, _ := repo.List(ctx, scope, "int-b", 10); len(rows) != 1 {
		t.Errorf("int-b rows = %d, want 1", len(rows))
	}
	if rows, _ := repo.List(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, "int-b", 10); len(rows) != 0 {
		t.Errorf("another environment sees %d rows", len(rows))
	}
}
