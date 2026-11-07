package store

import (
	"context"
	"errors"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

func TestTaskStatusRepo_Reopen(t *testing.T) {
	ctx := context.Background()
	repo := openTemp(t).TaskStatuses()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// No record: nothing to reopen, and none is created.
	if ok, err := repo.Reopen(ctx, scope, "t0", "again"); err != nil || ok {
		t.Fatalf("reopen of missing record = %v, %v, want false", ok, err)
	}
	if _, err := repo.Get(ctx, scope, "t0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reopen created a record: %v", err)
	}
	// Open: refused.
	if err := repo.Submit(ctx, scope, "t2"); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.Reopen(ctx, scope, "t2", "again"); err != nil || ok {
		t.Fatalf("reopen of open task = %v, %v, want false", ok, err)
	}

	if err := repo.Submit(ctx, scope, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(ctx, scope, "t1", agentdomain.TaskFailed, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.Reopen(ctx, scope, "t1", "again"); err != nil || !ok {
		t.Fatalf("reopen of failed task = %v, %v, want true", ok, err)
	}
	got, _ := repo.Get(ctx, scope, "t1")
	if got.State != agentdomain.TaskSubmitted || len(got.Notes) != 1 || got.Notes[0].Text != "again" {
		t.Fatalf("after reopen = %+v", got)
	}
	// Finished states other than failed are not reopened.
	if err := repo.Close(ctx, scope, "t1", agentdomain.TaskCompleted, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.Reopen(ctx, scope, "t1", "again"); ok {
		t.Fatal("completed task was reopened")
	}
}
