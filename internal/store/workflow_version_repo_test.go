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
	v1, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome","steps":[]}`), "[]", "", false)
	if err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	if v1.Seq != 1 {
		t.Fatalf("v1.Seq = %d, want 1", v1.Seq)
	}

	v2, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome","steps":[{"id":"S1"}]}`), `[{"stepId":"S1"}]`, "", false)
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
	v1, err := repo.Create(ctx, scopeA, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome"}`), "[]", "", false)
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

// TestWorkflowVersions_RetentionCap proves Create prunes old rows down to
// maxVersionsPerWorkflow inside the same transaction as the insert: after 55
// Creates on one workflow, exactly 50 rows remain, the retained set is the
// newest 50 (seqs 6..55), the latest (seq 55) is present, and seqs 1..5 are
// gone.
func TestWorkflowVersions_RetentionCap(t *testing.T) {
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
	var last WorkflowVersion
	for i := 0; i < 55; i++ {
		last, err = repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome","steps":[]}`), "[]", "", false)
		if err != nil {
			t.Fatalf("Create #%d: %v", i+1, err)
		}
	}
	if last.Seq != 55 {
		t.Fatalf("last.Seq = %d, want 55", last.Seq)
	}

	versions, err := repo.ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versions) != 50 {
		t.Fatalf("len(versions) = %d, want exactly 50", len(versions))
	}
	if versions[0].Seq != 6 {
		t.Fatalf("oldest retained seq = %d, want 6", versions[0].Seq)
	}
	if versions[len(versions)-1].Seq != 55 {
		t.Fatalf("newest retained seq = %d, want 55", versions[len(versions)-1].Seq)
	}
	seen := make(map[int]bool, len(versions))
	for _, v := range versions {
		seen[v.Seq] = true
	}
	for seq := 6; seq <= 55; seq++ {
		if !seen[seq] {
			t.Fatalf("seq %d missing from retained set", seq)
		}
	}
	for seq := 1; seq <= 5; seq++ {
		if seen[seq] {
			t.Fatalf("seq %d still present, want pruned", seq)
		}
	}

	// The compiled_dag_version pointer still targets the latest version even
	// after older rows are pruned.
	got, err := s.Workflows().GetByID(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.CompiledDAGVer != last.ID {
		t.Fatalf("CompiledDAGVer = %q, want latest version id %q", got.CompiledDAGVer, last.ID)
	}
}

// TestWorkflowVersions_RetentionCap_PerWorkflowIsolation proves the retention
// cap is scoped per workflow_id: pruning one workflow's version stack down to
// maxVersionsPerWorkflow must not touch another workflow's rows, even when
// both are published back-to-back.
func TestWorkflowVersions_RetentionCap_PerWorkflowIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}

	wfA, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Welcome A",
		TriggerIdentifier: "welcome-a",
		Steps:             json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("Workflows().Create A: %v", err)
	}
	wfB, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Welcome B",
		TriggerIdentifier: "welcome-b",
		Steps:             json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("Workflows().Create B: %v", err)
	}

	repo := s.WorkflowVersions()
	for i := 0; i < 55; i++ {
		if _, err := repo.Create(ctx, scope, wfA.ID, wfA.Name, wfA.TriggerIdentifier, json.RawMessage(`{"name":"a"}`), "[]", "", false); err != nil {
			t.Fatalf("Create A #%d: %v", i+1, err)
		}
	}
	for i := 0; i < 55; i++ {
		if _, err := repo.Create(ctx, scope, wfB.ID, wfB.Name, wfB.TriggerIdentifier, json.RawMessage(`{"name":"b"}`), "[]", "", false); err != nil {
			t.Fatalf("Create B #%d: %v", i+1, err)
		}
	}

	versionsA, err := repo.ListByWorkflow(ctx, scope, wfA.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow A: %v", err)
	}
	if len(versionsA) != 50 {
		t.Fatalf("len(versionsA) = %d, want 50", len(versionsA))
	}
	if versionsA[0].Seq != 6 || versionsA[len(versionsA)-1].Seq != 55 {
		t.Fatalf("versionsA range = [%d,%d], want [6,55]", versionsA[0].Seq, versionsA[len(versionsA)-1].Seq)
	}

	versionsB, err := repo.ListByWorkflow(ctx, scope, wfB.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow B: %v", err)
	}
	if len(versionsB) != 50 {
		t.Fatalf("len(versionsB) = %d, want 50", len(versionsB))
	}
	if versionsB[0].Seq != 6 || versionsB[len(versionsB)-1].Seq != 55 {
		t.Fatalf("versionsB range = [%d,%d], want [6,55]", versionsB[0].Seq, versionsB[len(versionsB)-1].Seq)
	}
}

// TestWorkflowVersions_UnderCap_NoPrune proves a workflow with fewer than
// maxVersionsPerWorkflow versions is left untouched: 10 Creates leave all 10
// rows in place, seq 1 included.
func TestWorkflowVersions_UnderCap_NoPrune(t *testing.T) {
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
	for i := 0; i < 10; i++ {
		if _, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome"}`), "[]", "", false); err != nil {
			t.Fatalf("Create #%d: %v", i+1, err)
		}
	}

	versions, err := repo.ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versions) != 10 {
		t.Fatalf("len(versions) = %d, want 10 (under cap, nothing pruned)", len(versions))
	}
	if versions[0].Seq != 1 {
		t.Fatalf("oldest seq = %d, want 1", versions[0].Seq)
	}
}

// TestWorkflowVersions_RollbackWithinRetainedWindow_StillResolves proves
// Rollback is unaffected by the retention cap as long as its target version
// id is still within the retained window: Get looks the row up by id, and
// pruning never removes a version inside the newest maxVersionsPerWorkflow.
func TestWorkflowVersions_RollbackWithinRetainedWindow_StillResolves(t *testing.T) {
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
	var versions []WorkflowVersion
	for i := 0; i < 55; i++ {
		v, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, json.RawMessage(`{"name":"env-a-welcome"}`), "[]", "", false)
		if err != nil {
			t.Fatalf("Create #%d: %v", i+1, err)
		}
		versions = append(versions, v)
	}

	// versions[5] is seq 6, the oldest still-retained row after the 55th
	// Create's prune (seqs 1..5 were dropped). It must still resolve.
	retained := versions[5]
	got, err := repo.Get(ctx, scope, retained.ID)
	if err != nil {
		t.Fatalf("Get retained version (seq %d): %v", retained.Seq, err)
	}
	if got.Seq != 6 {
		t.Fatalf("got.Seq = %d, want 6", got.Seq)
	}

	// versions[0] is seq 1, pruned by the cap. Its id no longer resolves —
	// this is the documented, acceptable retention-window limit on how far
	// back a rollback can target.
	pruned := versions[0]
	if _, err := repo.Get(ctx, scope, pruned.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get pruned version err = %v, want ErrNotFound", err)
	}
}

// TestWorkflowVersionRepo_Create_PersistsSnapshotFields proves migration
// 0028's steps/payload_schema/critical columns round-trip through Create and
// every read path (Get, ListByWorkflow, LatestByWorkflow): a version is a
// full snapshot of the editable workflow row, not just the compiled def.
func TestWorkflowVersionRepo_Create_PersistsSnapshotFields(t *testing.T) {
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

	const (
		wantSteps         = `[{"stepId":"S1","type":"in_app","controlValues":{"body":"hi"}}]`
		wantPayloadSchema = `{"type":"object"}`
	)
	repo := s.WorkflowVersions()
	v1, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier,
		json.RawMessage(`{"name":"env-a-welcome","steps":[{"id":"S1"}]}`), wantSteps, wantPayloadSchema, true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v1.Steps != wantSteps {
		t.Fatalf("Create result Steps = %q, want %q", v1.Steps, wantSteps)
	}
	if v1.PayloadSchema != wantPayloadSchema {
		t.Fatalf("Create result PayloadSchema = %q, want %q", v1.PayloadSchema, wantPayloadSchema)
	}
	if !v1.Critical {
		t.Fatal("Create result Critical = false, want true")
	}

	got, err := repo.Get(ctx, scope, v1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Steps != wantSteps || got.PayloadSchema != wantPayloadSchema || !got.Critical {
		t.Fatalf("Get = %+v, want Steps=%q PayloadSchema=%q Critical=true", got, wantSteps, wantPayloadSchema)
	}

	listed, err := repo.ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(listed) != 1 || listed[0].Steps != wantSteps || listed[0].PayloadSchema != wantPayloadSchema || !listed[0].Critical {
		t.Fatalf("ListByWorkflow = %+v, want one row with Steps=%q PayloadSchema=%q Critical=true", listed, wantSteps, wantPayloadSchema)
	}

	latest, err := repo.LatestByWorkflow(ctx, wf.ID)
	if err != nil {
		t.Fatalf("LatestByWorkflow: %v", err)
	}
	if latest.Steps != wantSteps || latest.PayloadSchema != wantPayloadSchema || !latest.Critical {
		t.Fatalf("LatestByWorkflow = %+v, want Steps=%q PayloadSchema=%q Critical=true", latest, wantSteps, wantPayloadSchema)
	}
}

// TestWorkflowVersionRepo_Create_EmptySnapshotFieldsMeansPre0028 proves a
// version created with an empty steps string (simulating a row captured
// before migration 0028 introduced the snapshot columns) round-trips as
// empty rather than erroring or substituting a default — this is the exact
// signal authoring.Service.Rollback uses to detect a pre-0028 version and
// fall back to its old compiled_def-only restore behavior.
func TestWorkflowVersionRepo_Create_EmptySnapshotFieldsMeansPre0028(t *testing.T) {
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
	v1, err := repo.Create(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier,
		json.RawMessage(`{"name":"env-a-welcome"}`), "", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v1.Steps != "" {
		t.Fatalf("Steps = %q, want empty (pre-0028 signal)", v1.Steps)
	}

	got, err := repo.Get(ctx, scope, v1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Steps != "" {
		t.Fatalf("Get Steps = %q, want empty (pre-0028 signal)", got.Steps)
	}
}
