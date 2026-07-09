package authoring_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

// fakeRegisterer captures every RegisterWorkflow call for assertions,
// standing in for a live engine so version/rollback tests can assert exactly
// what was registered without booting a real dagnats sidecar (engine.Engine
// exposes no RegisterWorkflow introspection API of its own). authoring.New's
// eng parameter is satisfied structurally: it only needs a RegisterWorkflow
// method with this signature.
type fakeRegisterer struct {
	calls []engine.WorkflowDef
}

func (f *fakeRegisterer) RegisterWorkflow(_ context.Context, def engine.WorkflowDef) error {
	f.calls = append(f.calls, def)
	return nil
}

func (f *fakeRegisterer) last() engine.WorkflowDef {
	return f.calls[len(f.calls)-1]
}

func newVersioningTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "authoring-versioning.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestPublish_RecordsExactlyOneVersion proves Publish appends exactly one
// workflow_versions row per call, carrying the exact def RegisterWorkflow was
// called with.
func TestPublish_RecordsExactlyOneVersion(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	wf, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("RegisterWorkflow calls = %d, want 1", len(fake.calls))
	}

	versions, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}

	var got engine.WorkflowDef
	if err := json.Unmarshal(versions[0].CompiledDef, &got); err != nil {
		t.Fatalf("unmarshal compiled_def: %v", err)
	}
	if !reflect.DeepEqual(got, fake.last()) {
		t.Fatalf("stored compiled_def = %+v, want registered def %+v", got, fake.last())
	}
	if versions[0].Seq != 1 {
		t.Fatalf("Seq = %d, want 1", versions[0].Seq)
	}
}

// TestPublish_TwoPublishesIncreasingSeq proves two Publish calls on the same
// workflow append two rows with increasing Seq, not overwrite one row.
func TestPublish_TwoPublishesIncreasingSeq(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	wf, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}

	versions, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	if versions[0].Seq != 1 || versions[1].Seq != 2 {
		t.Fatalf("seqs = [%d,%d], want [1,2]", versions[0].Seq, versions[1].Seq)
	}
	if versions[0].ID == versions[1].ID {
		t.Fatal("two Publishes produced the same version id; want two distinct rows")
	}
}

// TestRollback_ReregistersEarlierVersionByteForByte proves Rollback loads the
// target version's compiled_def, re-registers it EXACTLY (skipping Compile —
// a later edit changed the workflow's live steps, but the rolled-back def
// must match the original one-step version, not the current two-step one),
// and appends a new version row copying that same def onto the stack.
func TestRollback_ReregistersEarlierVersionByteForByte(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}
	v1Def := fake.last()

	wf, err := svc.Update(ctx, scope, "welcome", authoring.UpdateInput{
		Name: "Welcome",
		Steps: []domain.Step{
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
			{StepID: "S2", Type: "in_app", ControlValues: json.RawMessage(`{"body":"bye"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v2: %v", err)
	}
	v2Def := fake.last()
	if len(v2Def.Steps) != 2 {
		t.Fatalf("v2 steps = %d, want 2 (sanity: v1 and v2 must differ)", len(v2Def.Steps))
	}

	versions, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions before rollback = %d, want 2", len(versions))
	}
	v1ID := versions[0].ID

	if err := svc.Rollback(ctx, scope, "welcome", v1ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// RegisterWorkflow must have been called once more by Rollback, with v1's
	// exact def (1 step) — not a recompile of the workflow's now-2-step live
	// steps. Prior calls: Publish v1 (1), Update's own re-register (2),
	// Publish v2 (3); Rollback adds the 4th.
	if len(fake.calls) != 4 {
		t.Fatalf("RegisterWorkflow calls after Rollback = %d, want 4", len(fake.calls))
	}
	rolledBackDef := fake.last()
	if !reflect.DeepEqual(rolledBackDef, v1Def) {
		t.Fatalf("Rollback registered %+v, want v1's exact def %+v", rolledBackDef, v1Def)
	}
	if reflect.DeepEqual(rolledBackDef, v2Def) {
		t.Fatal("Rollback registered v2's def; want v1's")
	}

	// Rollback is itself a publish: the stack grows to 3, and the newest row
	// carries v1's def again (append-only, auditable).
	versionsAfter, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow after rollback: %v", err)
	}
	if len(versionsAfter) != 3 {
		t.Fatalf("versions after rollback = %d, want 3", len(versionsAfter))
	}
	if versionsAfter[2].Seq != 3 {
		t.Fatalf("newest version Seq = %d, want 3", versionsAfter[2].Seq)
	}
	var newestDef engine.WorkflowDef
	if err := json.Unmarshal(versionsAfter[2].CompiledDef, &newestDef); err != nil {
		t.Fatalf("unmarshal newest compiled_def: %v", err)
	}
	if !reflect.DeepEqual(newestDef, v1Def) {
		t.Fatalf("newest version compiled_def = %+v, want v1's def %+v", newestDef, v1Def)
	}
}

// TestRepublishAll_RollbackSurvivesRestart is the durability regression: a
// rollback must not be silently undone by the next boot. Publish v1, edit +
// publish v2, roll back to v1 (which appends v3 = v1's def as the new latest
// version), then simulate a process restart by running RepublishAll against a
// FRESH capturing engine (the real engine loses its in-memory registry on
// restart). RepublishAll must re-register v1's def — the latest version's
// compiled_def byte-for-byte — NOT recompile v2 from the live workflows.steps.
// Against the old RepublishAll (which recompiled wf.steps and never read
// workflow_versions) this asserts the 2-step v2 def and fails.
func TestRepublishAll_RollbackSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}
	v1Def := fake.last()

	wf, err := svc.Update(ctx, scope, "welcome", authoring.UpdateInput{
		Name: "Welcome",
		Steps: []domain.Step{
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
			{StepID: "S2", Type: "in_app", ControlValues: json.RawMessage(`{"body":"bye"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v2: %v", err)
	}
	v2Def := fake.last()

	versions, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	v1ID := versions[0].ID
	if err := svc.Rollback(ctx, scope, "welcome", v1ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Simulate a restart: fresh engine with an empty registry, fresh service
	// over the SAME store.
	bootFake := &fakeRegisterer{}
	bootSvc := authoring.New(st, bootFake)
	n, err := bootSvc.RepublishAll(ctx)
	if err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("RepublishAll registered %d, want 1", n)
	}
	if len(bootFake.calls) != 1 {
		t.Fatalf("boot RegisterWorkflow calls = %d, want 1", len(bootFake.calls))
	}
	booted := bootFake.calls[0]
	if !reflect.DeepEqual(booted, v1Def) {
		t.Fatalf("boot registered %+v, want v1's def %+v (rollback did not survive restart)", booted, v1Def)
	}
	if reflect.DeepEqual(booted, v2Def) {
		t.Fatal("boot registered v2's def; rollback was silently undone by RepublishAll")
	}
}

// TestRepublishAll_NoVersionsFallsBackToCompile proves the backward-compat
// path: a workflow with NO version rows (created but never published, or a
// pre-migration row) is still re-registered at boot by compiling its live
// steps — the fallback is intact.
func TestRepublishAll_NoVersionsFallsBackToCompile(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	// Create persists a row WITHOUT publishing, so no workflow_versions row
	// exists for it.
	svc := authoring.New(st, &fakeRegisterer{})
	wf, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if versions, vErr := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID); vErr != nil || len(versions) != 0 {
		t.Fatalf("precondition: versions=%v err=%v, want 0 versions", versions, vErr)
	}

	bootFake := &fakeRegisterer{}
	bootSvc := authoring.New(st, bootFake)
	n, err := bootSvc.RepublishAll(ctx)
	if err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("RepublishAll registered %d, want 1 (compile fallback)", n)
	}
	if len(bootFake.calls) != 1 {
		t.Fatalf("boot RegisterWorkflow calls = %d, want 1", len(bootFake.calls))
	}
	// The fallback recompiles the live 1-step workflow.
	if got := bootFake.calls[0]; got.Name != "env_1-welcome" || len(got.Steps) != 1 {
		t.Fatalf("fallback registered %+v, want env_1-welcome with 1 step", got)
	}
}

// TestRollback_CrossTenantVersionID404s proves a versionID belonging to a
// different tenant's workflow is rejected as not-found even when the calling
// scope has its own workflow under the same trigger identifier — the version
// lookup is scope-checked via the workflow_id -> workflows join, not just by
// raw id.
func TestRollback_CrossTenantVersionID404s(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scopeA := domain.Scope{OrgID: "org_1", EnvID: "env_a"}
	scopeB := domain.Scope{OrgID: "org_1", EnvID: "env_b"}

	// Same trigger identifier exists in both scopes as distinct workflows.
	if _, err := svc.Create(ctx, scopeA, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create scopeA: %v", err)
	}
	if err := svc.Publish(ctx, scopeA, "welcome"); err != nil {
		t.Fatalf("Publish scopeA: %v", err)
	}

	if _, err := svc.Create(ctx, scopeB, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create scopeB: %v", err)
	}

	wfA, err := svc.GetBySlug(ctx, scopeA, "welcome")
	if err != nil {
		t.Fatalf("GetBySlug scopeA: %v", err)
	}
	versionsA, err := st.WorkflowVersions().ListByWorkflow(ctx, scopeA, wfA.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow scopeA: %v", err)
	}
	if len(versionsA) != 1 {
		t.Fatalf("scopeA versions = %d, want 1", len(versionsA))
	}
	crossTenantVersionID := versionsA[0].ID

	err = svc.Rollback(ctx, scopeB, "welcome", crossTenantVersionID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Rollback cross-tenant version err = %v, want store.ErrNotFound", err)
	}

	// scopeB's own registration count must be unaffected by the rejected
	// rollback attempt — only its own Publish call should have registered.
	registeredForB := 0
	for _, def := range fake.calls {
		if def.Name == "env_b-welcome" {
			registeredForB++
		}
	}
	if registeredForB != 0 {
		t.Fatalf("cross-tenant Rollback registered %d defs for scopeB, want 0 (scopeB never published)", registeredForB)
	}
}
