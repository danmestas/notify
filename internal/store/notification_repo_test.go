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
