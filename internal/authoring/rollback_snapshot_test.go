package authoring_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// TestRollback_RestoresEditableRowFullSnapshot is the H8 fix proof: publish
// v1, edit the workflow's steps/name/critical and publish v2, then roll back
// to v1. Before migration 0028, Rollback only re-registered v1's compiled
// def and left workflows.steps/name/payload_schema/critical at v2's values —
// a hybrid of v1 topology and v2 content. This asserts the whole editable
// row now matches v1 exactly, the engine was registered with v1's exact def,
// and a new v3 was appended copying v1's full snapshot (append-only stack
// preserved).
func TestRollback_RestoresEditableRowFullSnapshot(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome v1",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi v1"}`)},
		},
		Critical: false,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}

	wf1, err := svc.GetBySlug(ctx, scope, "welcome")
	if err != nil {
		t.Fatalf("GetBySlug after Publish v1: %v", err)
	}

	wf, err := svc.Update(ctx, scope, "welcome", authoring.UpdateInput{
		Name: "Welcome v2",
		Steps: []domain.Step{
			// S1 renamed to S2 between v1 and v2 — exactly the scenario H8
			// flags: a rolled-back v1 def references stepId "S1", which must
			// no longer exist only in the (stale, pre-fix) live steps.
			{StepID: "S2", Type: "in_app", ControlValues: json.RawMessage(`{"body":"bye v2"}`)},
		},
		PayloadSchema: json.RawMessage(`{"type":"object","properties":{"v":{}}}`),
		Critical:      true,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v2: %v", err)
	}

	versions, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	// Create (0), Publish v1 (1), Update (records its own version, 2), Publish
	// v2 (3): Update now records a version too, so the stack is 3 before
	// rollback. versions[0] is still Publish v1's full snapshot.
	if len(versions) != 3 {
		t.Fatalf("versions before rollback = %d, want 3", len(versions))
	}
	v1 := versions[0]

	if err := svc.Rollback(ctx, scope, "welcome", v1.ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The live editable row must now match v1 exactly: name, payload_schema,
	// critical, and step content (not just count).
	got, err := svc.GetBySlug(ctx, scope, "welcome")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.Name != wf1.Name {
		t.Fatalf("Name = %q, want v1's %q", got.Name, wf1.Name)
	}
	if got.Critical != wf1.Critical {
		t.Fatalf("Critical = %v, want v1's %v", got.Critical, wf1.Critical)
	}
	if string(got.PayloadSchema) != string(wf1.PayloadSchema) {
		t.Fatalf("PayloadSchema = %q, want v1's %q", got.PayloadSchema, wf1.PayloadSchema)
	}
	var gotSteps []domain.Step
	if err := json.Unmarshal(got.Steps, &gotSteps); err != nil {
		t.Fatalf("unmarshal restored steps: %v", err)
	}
	if len(gotSteps) != 1 || gotSteps[0].StepID != "S1" {
		t.Fatalf("restored steps = %+v, want single step S1 (v1's topology)", gotSteps)
	}
	var controls struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(gotSteps[0].ControlValues, &controls); err != nil {
		t.Fatalf("unmarshal step controlValues: %v", err)
	}
	if controls.Body != "hi v1" {
		t.Fatalf("restored step body = %q, want v1's %q", controls.Body, "hi v1")
	}

	// The engine must have been registered with v1's exact compiled def.
	var v1Def engine.WorkflowDef
	if err := json.Unmarshal(v1.CompiledDef, &v1Def); err != nil {
		t.Fatalf("unmarshal v1 compiled_def: %v", err)
	}
	if !reflect.DeepEqual(fake.last(), v1Def) {
		t.Fatalf("registered def = %+v, want v1's def %+v", fake.last(), v1Def)
	}

	// A new v3 was appended, copying v1's full snapshot — append-only stack
	// preserved, not a mutation of history.
	versionsAfter, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow after rollback: %v", err)
	}
	if len(versionsAfter) != 4 {
		t.Fatalf("versions after rollback = %d, want 4", len(versionsAfter))
	}
	v3 := versionsAfter[3]
	if v3.Steps != v1.Steps {
		t.Fatalf("v3.Steps = %q, want v1.Steps %q", v3.Steps, v1.Steps)
	}
	if v3.PayloadSchema != v1.PayloadSchema || v3.Critical != v1.Critical {
		t.Fatalf("v3 snapshot = %+v, want to match v1 %+v", v3, v1)
	}
}

// TestRollback_CorrectnessProof_DeliveryContentMatchesRolledBackTopology is
// the H8 correctness proof, not just a UI-honesty check: after Rollback,
// delivery's step-content lookup (internal/delivery.RebuildEnvelope, which
// reads workflows.steps by StepID at send time — not the compiled def) must
// resolve the ROLLED-BACK version's step id and content. Before this fix, a
// rollback re-registered v1's compiled_def (which references stepId "S1")
// while workflows.steps stayed at v2's ("S2" only) — so a run driven by the
// rolled-back v1 topology would call RebuildEnvelope with StepID "S1", which
// could not be found in the live (v2) steps, erroring "step \"S1\" not found
// in workflow". This test drives the real delivery.RebuildEnvelope function
// against a run whose StepID is v1's "S1" and asserts it resolves — with v1's
// content, not v2's — proving engine topology and delivery content agree
// post-rollback.
func TestRollback_CorrectnessProof_DeliveryContentMatchesRolledBackTopology(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi v1"}`)},
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}

	wf, err := svc.Update(ctx, scope, "welcome", authoring.UpdateInput{
		Name: "Welcome",
		Steps: []domain.Step{
			{StepID: "S2", Type: "in_app", ControlValues: json.RawMessage(`{"body":"bye v2"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish v2: %v", err)
	}

	versions, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	v1ID := versions[0].ID

	if err := svc.Rollback(ctx, scope, "welcome", v1ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Seed a subscriber and a notification/run pointing at this workflow, as
	// delivery would have at send time.
	sub, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "sub-1", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("Subscribers().Upsert: %v", err)
	}
	notif, err := st.Notifications().Create(ctx, scope, domain.Notification{
		WorkflowID:    wf.ID,
		SubscriberID:  sub.SubscriberID,
		TransactionID: "txn-1",
		RunID:         "run-1",
		Payload:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Notifications().Create: %v", err)
	}

	// The engine, post-rollback, drives step "S1" (v1's topology). Delivery
	// must resolve it from the live row and return v1's content.
	env, _, _, err := delivery.RebuildEnvelope(ctx, st, engine.TaskPayload{RunID: notif.RunID, StepID: "S1"})
	if err != nil {
		t.Fatalf("RebuildEnvelope for rolled-back step S1: %v (H8 regression: rollback restored topology but not content)", err)
	}
	if env.Step.BodyTemplate != "hi v1" {
		t.Fatalf("Step.BodyTemplate = %q, want v1's %q", env.Step.BodyTemplate, "hi v1")
	}
}

// TestRollback_Pre0028Version_FallsBackWithoutRestoringRow proves a version
// row captured before migration 0028 (steps empty — the column defaults
// empty on ADD COLUMN) does not crash Rollback and does not blank the live
// editable row: it registers the compiled def only, exactly as Rollback
// behaved before this fix.
func TestRollback_Pre0028Version_FallsBackWithoutRestoringRow(t *testing.T) {
	ctx := context.Background()
	st := newVersioningTestStore(t)
	fake := &fakeRegisterer{}
	svc := authoring.New(st, fake)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	wf, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"live"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a version row captured before migration 0028: steps/
	// payload_schema empty, critical false — only compiled_def + audit
	// metadata present, exactly what a pre-0028 row looks like after the ADD
	// COLUMN default applies.
	legacyDef := engine.WorkflowDef{
		Name:  "env_1-welcome",
		Steps: []engine.StepDef{{ID: "S1", Task: "send.in_app", Type: "normal"}},
	}
	legacyDefJSON, err := json.Marshal(legacyDef)
	if err != nil {
		t.Fatalf("marshal legacyDef: %v", err)
	}
	legacyVer, err := st.WorkflowVersions().Create(ctx, scope, wf.ID, "Welcome (legacy)", "welcome", legacyDefJSON, "", "", false)
	if err != nil {
		t.Fatalf("Create legacy version: %v", err)
	}

	if err := svc.Rollback(ctx, scope, "welcome", legacyVer.ID); err != nil {
		t.Fatalf("Rollback to pre-0028 version: %v (must not crash)", err)
	}

	// The engine was registered with the legacy compiled def.
	if len(fake.calls) != 1 {
		t.Fatalf("RegisterWorkflow calls = %d, want 1", len(fake.calls))
	}
	if !reflect.DeepEqual(fake.last(), legacyDef) {
		t.Fatalf("registered def = %+v, want legacy def %+v", fake.last(), legacyDef)
	}

	// The editable row must be UNCHANGED — the fallback leaves it as-is
	// rather than blanking steps.
	got, err := svc.GetBySlug(ctx, scope, "welcome")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.Name != "Welcome" {
		t.Fatalf("Name = %q, want unchanged %q", got.Name, "Welcome")
	}
	var gotSteps []domain.Step
	if err := json.Unmarshal(got.Steps, &gotSteps); err != nil {
		t.Fatalf("unmarshal steps: %v", err)
	}
	if len(gotSteps) != 1 || gotSteps[0].StepID != "S1" {
		t.Fatalf("steps = %+v, want unchanged single step S1", gotSteps)
	}
	var controls struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(gotSteps[0].ControlValues, &controls); err != nil {
		t.Fatalf("unmarshal controlValues: %v", err)
	}
	if controls.Body != "live" {
		t.Fatalf("step body = %q, want unchanged %q", controls.Body, "live")
	}

	// Rollback still appends a new version row (mirrors the pre-fix
	// behavior: rollback is itself a publish).
	versionsAfter, err := st.WorkflowVersions().ListByWorkflow(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow: %v", err)
	}
	if len(versionsAfter) != 2 {
		t.Fatalf("versions after rollback = %d, want 2 (legacy + rollback's)", len(versionsAfter))
	}
}
