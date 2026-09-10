package store

import (
	"context"
	"testing"
)

// TestWebhookDeliveryRepo_ClaimIsExclusive: a due row can be claimed once;
// the claim hides it from Due until the lease expires, and Reschedule /
// Delete behave as the worker expects.
func TestWebhookDeliveryRepo_ClaimIsExclusive(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	repo := s.WebhookDeliveries()
	if err := repo.Enqueue(ctx, []WebhookDelivery{{OrgID: "o", EnvID: "e", EndpointID: "ep", Payload: []byte(`{}`), NextAttemptAt: "2026-01-01T00:00:00.000Z"}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, err := repo.Due(ctx, "2026-01-01T00:00:01.000Z", 10)
	if err != nil || len(due) != 1 || due[0].ID == "" || due[0].Scope().OrgID != "o" {
		t.Fatalf("Due = %+v, %v", due, err)
	}
	id := due[0].ID
	if ok, err := repo.Claim(ctx, id, "2026-01-01T00:00:01.000Z", "2026-01-01T00:01:01.000Z"); err != nil || !ok {
		t.Fatalf("first Claim = %v, %v, want true", ok, err)
	}
	if ok, _ := repo.Claim(ctx, id, "2026-01-01T00:00:01.000Z", "2026-01-01T00:01:01.000Z"); ok {
		t.Fatal("second Claim succeeded on a row another worker holds")
	}
	if due, _ := repo.Due(ctx, "2026-01-01T00:00:02.000Z", 10); len(due) != 0 {
		t.Fatalf("Due during the lease = %d rows, want 0", len(due))
	}
	if due, _ := repo.Due(ctx, "2026-01-01T00:02:00.000Z", 10); len(due) != 1 {
		t.Fatal("row not due again after the lease")
	}
	if err := repo.Reschedule(ctx, id, 3, "2026-01-01T01:00:00.000Z"); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	due, _ = repo.Due(ctx, "2026-01-01T01:00:00.000Z", 10)
	if len(due) != 1 || due[0].Attempts != 3 {
		t.Fatalf("after Reschedule: %+v", due)
	}
	if err := repo.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := repo.Delete(ctx, id); err != nil {
		t.Fatalf("Delete of a missing row: %v", err)
	}
	if due, _ := repo.Due(ctx, "2026-12-31T00:00:00.000Z", 10); len(due) != 0 {
		t.Fatal("row survived Delete")
	}
}
