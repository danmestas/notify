package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestWorkflowVersionRepo_CreateAllocatesIncreasingSeq proves Create computes
// Seq atomically as COALESCE(MAX(seq),0)+1: two Creates on the same workflow
// get seq 1 then seq 2, and the UNIQUE (workflow_id, seq) index (migration
// 0027) never trips for the sequential (non-racing) case.
func TestWorkflowVersionRepo_CreateAllocatesIncreasingSeq(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}

	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}

	repo := s.WorkflowVersions()
	v1, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome","steps":[]}`))
	if err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	if v1.Seq != 1 {
		t.Fatalf("v1.Seq = %d, want 1", v1.Seq)
	}

	v2, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome","steps":[{"id":"S1"}]}`))
	if err != nil {
		t.Fatalf("Create v2: %v", err)
	}
	if v2.Seq != 2 {
		t.Fatalf("v2.Seq = %d, want 2", v2.Seq)
	}

	versions, err := repo.ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(versions))
	}
	if versions[0].Seq != 1 || versions[1].Seq != 2 {
		t.Fatalf("versions seq order = [%d,%d], want [1,2]", versions[0].Seq, versions[1].Seq)
	}

	// Create repoints the retired compiled_dag_version marker at the latest
	// version's id rather than leaving it a dead, always-empty column.
	got, err := s.Workflows().GetByID(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.CompiledDAGVer != v2.ID {
		t.Fatalf("CompiledDAGVer = %q, want latest version id %q", got.CompiledDAGVer, v2.ID)
	}
}

// TestWorkflowVersionRepo_Get_CrossTenant404s proves Get's workflow_id ->
// workflows join scope-checks the version: a version id that is valid in one
// tenant's org/env is invisible (ErrNotFound) from a different org/env scope.
func TestWorkflowVersionRepo_Get_CrossTenant404s(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scopeA := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	scopeB := domain.Scope{OrgID: "org-1", EnvID: "env-b"}

	wf, err := s.Workflows().Create(ctx, scopeA, domain.Workflow{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("Workflows().Create: %v", err)
	}

	repo := s.WorkflowVersions()
	v1, err := repo.Create(ctx, scopeA, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := repo.Get(ctx, scopeA, v1.ID); err != nil {
		t.Fatalf("Get within scope: %v", err)
	}
	if _, err := repo.Get(ctx, scopeB, v1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env Get err = %v, want ErrNotFound", err)
	}
}
