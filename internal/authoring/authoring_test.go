package authoring_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

func TestDAGName(t *testing.T) {
	got := authoring.DAGName(domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "welcome")
	if want := "env_1-welcome"; got != want {
		t.Fatalf("DAGName = %q, want %q", got, want)
	}
}

func TestCompile_OneStepInApp(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "welcome",
		Steps:             mustSteps(t, []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if def.Name != "env_1-welcome" {
		t.Fatalf("def.Name = %q, want env_1-welcome", def.Name)
	}
	if len(def.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(def.Steps))
	}
	s := def.Steps[0]
	if s.ID != "S1" || s.Task != "send.in_app" || s.Type != "normal" {
		t.Fatalf("step = %+v, want {ID:S1 Task:send.in_app Type:normal}", s)
	}
	if len(s.DependsOn) != 0 {
		t.Fatalf("root step DependsOn = %v, want empty", s.DependsOn)
	}
}

// TestCompile_SetsIdempotencyKey proves every compiled DAG dedupes retried
// triggers by the envelope's top-level idempotencyKey field. trigger.go
// composes runenv.InAppEnvelope.IdempotencyKey (json:"idempotencyKey",
// top-level, NOT nested under payload) from (TransactionID, SubscriberID) —
// the shape shared by every channel DAG — so the dagnats admission check
// (StartRun: a repeated key value returns the existing run id instead of
// starting a duplicate) fires for every workflow. Composing on the recipient
// too (not TransactionID alone) matters because a multicast trigger shares
// one TransactionID across every fan-out recipient; see
// runenv.InAppEnvelope.IdempotencyKey's doc comment.
func TestCompile_SetsIdempotencyKey(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "welcome",
		Steps:             mustSteps(t, []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if def.IdempotencyKey != "idempotencyKey" {
		t.Fatalf("def.IdempotencyKey = %q, want %q", def.IdempotencyKey, "idempotencyKey")
	}
}

func TestCompile_TwoStepChains(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "welcome",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "S1", Type: "in_app"},
			{StepID: "S2", Type: "in_app"},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(def.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(def.Steps))
	}
	if len(def.Steps[0].DependsOn) != 0 {
		t.Fatalf("step1 DependsOn = %v, want empty", def.Steps[0].DependsOn)
	}
	dep := def.Steps[1].DependsOn
	if len(dep) != 1 || dep[0] != "S1" {
		t.Fatalf("step2 DependsOn = %v, want [S1]", dep)
	}
}

func TestCompile_DelayThenInApp(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "welcome",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"hi","body":"there"}`)},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(def.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(def.Steps))
	}
	d := def.Steps[0]
	if d.ID != "d1" || d.Type != "sleep" {
		t.Fatalf("delay step = %+v, want {ID:d1 Type:sleep}", d)
	}
	if d.Task != "" {
		t.Fatalf("sleep step Task = %q, want empty", d.Task)
	}
	if string(d.Config) != `{"duration":1000000000}` {
		t.Fatalf("sleep config = %s, want {\"duration\":1000000000}", d.Config)
	}
	if len(d.DependsOn) != 0 {
		t.Fatalf("root sleep DependsOn = %v, want empty", d.DependsOn)
	}
	c := def.Steps[1]
	if c.ID != "S1" || c.Task != "send.in_app" || c.Type != "normal" {
		t.Fatalf("channel step = %+v, want {ID:S1 Task:send.in_app Type:normal}", c)
	}
	if len(c.DependsOn) != 1 || c.DependsOn[0] != "d1" {
		t.Fatalf("channel DependsOn = %v, want [d1]", c.DependsOn)
	}
}

func TestCompile_DigestThenInApp(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "digested",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Digest","body":"You have {{digest.count}} new items"}`)},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(def.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(def.Steps))
	}
	d := def.Steps[0]
	if d.ID != "dg" || d.Type != "sleep" || d.Task != "" {
		t.Fatalf("digest step = %+v, want {ID:dg Type:sleep Task:\"\"}", d)
	}
	if string(d.Config) != `{"duration":1000000000}` {
		t.Fatalf("sleep config = %s, want {\"duration\":1000000000}", d.Config)
	}
	if len(d.DependsOn) != 0 {
		t.Fatalf("root sleep DependsOn = %v, want empty", d.DependsOn)
	}
	c := def.Steps[1]
	if c.ID != "S1" || c.Task != "send.in_app" || c.Type != "normal" {
		t.Fatalf("channel step = %+v, want {ID:S1 Task:send.in_app Type:normal}", c)
	}
	if len(c.DependsOn) != 1 || c.DependsOn[0] != "dg" {
		t.Fatalf("channel DependsOn = %v, want [dg]", c.DependsOn)
	}
}

// TestCompile_DigestScheduledThenInApp proves a scheduled (cron) digest
// compiles to a normal digest.wait task (no static sleep Config) with the
// downstream channel step depending on it.
func TestCompile_DigestScheduledThenInApp(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "scheduled",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"0 * * * *"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Digest","body":"You have {{digest.count}} new items"}`)},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	d := def.Steps[0]
	if d.ID != "dg" || d.Type != "normal" || d.Task != "digest.wait" {
		t.Fatalf("scheduled digest step = %+v, want {ID:dg Type:normal Task:digest.wait}", d)
	}
	if d.Config != nil {
		t.Fatalf("scheduled digest Config = %s, want nil", d.Config)
	}
	c := def.Steps[1]
	if len(c.DependsOn) != 1 || c.DependsOn[0] != "dg" {
		t.Fatalf("channel DependsOn = %v, want [dg]", c.DependsOn)
	}
}

// TestCompile_DigestScheduled_InvalidCron proves an unparseable cron expression
// fails compilation so authoring never registers a broken DAG.
func TestCompile_DigestScheduled_InvalidCron(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "scheduled",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"not-a-cron"}`)},
		}),
	}
	if _, err := authoring.Compile(wf); err == nil {
		t.Fatal("Compile invalid scheduled cron: want error, got nil")
	}
}

// TestCreate_RejectsInvalidScheduledCron proves authoring.Create maps a bad
// scheduled-digest cron to ErrCompileFailure (HTTP 422) BEFORE any workflow row
// is written — the 422 gate fires at save time, not at run time.
func TestCreate_RejectsInvalidScheduledCron(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, nil)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	_, err = svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Bad",
		TriggerIdentifier: "bad-cron",
		Steps: []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"nope"}`)},
		},
	})
	var ce *authoring.ErrCompileFailure
	if !errors.As(err, &ce) {
		t.Fatalf("Create error = %v, want *authoring.ErrCompileFailure", err)
	}
	if _, gErr := svc.GetBySlug(ctx, scope, "bad-cron"); gErr == nil {
		t.Fatal("workflow row was written despite compile failure; want no row")
	}
}

func TestCompile_DelayUnits(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "welcome",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"amount":2,"unit":"minutes"}`)},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if string(def.Steps[0].Config) != `{"duration":120000000000}` {
		t.Fatalf("sleep config = %s, want {\"duration\":120000000000}", def.Steps[0].Config)
	}
}

func TestPublish_DelayWorkflowRegisters(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Delayed",
		TriggerIdentifier: "delayed",
		Steps: []domain.Step{
			{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Publish(ctx, scope, "delayed"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestPublish_RegistersWithEngine(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func mustSteps(t *testing.T, steps []domain.Step) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	return b
}

// TestRepublishAll_RegistersAllStoredWorkflows seeds workflows across two
// scopes and asserts RepublishAll re-registers every one of them with the
// engine. This is the boot-time path that survives an engine restart: without
// it, persisted workflows fire with task_count=0 until re-saved.
func TestRepublishAll_RegistersAllStoredWorkflows(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)

	// Two scopes, three workflows total — proves ListAll spans org/env.
	scopeA := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	scopeB := domain.Scope{OrgID: "org_2", EnvID: "env_2"}
	seed := func(scope domain.Scope, name, slug string) {
		t.Helper()
		if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
			Name:              name,
			TriggerIdentifier: slug,
			Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
		}); err != nil {
			t.Fatalf("Create %s: %v", slug, err)
		}
	}
	seed(scopeA, "Welcome", "welcome")
	seed(scopeA, "Reminder", "reminder")
	seed(scopeB, "Invite", "invite")

	// A fresh engine has no in-memory registry: RepublishAll must re-register
	// all three. Create persists rows; it does not depend on the engine
	// retaining them, so this models the post-restart state.
	n, err := svc.RepublishAll(ctx)
	if err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	if n != 3 {
		t.Fatalf("RepublishAll registered %d workflows, want 3", n)
	}
}

// TestRepublishAll_SkipsUncompilableWorkflow proves resilience: a stored
// workflow with steps that fail Compile (malformed delay) is logged-and-skipped
// rather than aborting the whole re-registration. The valid workflow still gets
// registered. The bad row is written directly through the store to bypass
// Create's compile-on-write validation (which would reject it up front).
func TestRepublishAll_SkipsUncompilableWorkflow(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	// One valid workflow via the normal path.
	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create valid: %v", err)
	}

	// One INVALID workflow written straight to the store (delay amount <= 0
	// fails Compile). Create would reject this, so go through the repo directly.
	bad := domain.Workflow{
		Name:              "Broken",
		TriggerIdentifier: "broken",
		Active:            true,
		Steps:             mustSteps(t, []domain.Step{{StepID: "D1", Type: "delay", ControlValues: json.RawMessage(`{"amount":0,"unit":"seconds"}`)}}),
	}
	if _, err := st.Workflows().Create(ctx, scope, bad); err != nil {
		t.Fatalf("seed bad workflow: %v", err)
	}

	// RepublishAll must not abort on the bad one; it registers the good one.
	n, err := svc.RepublishAll(ctx)
	if err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("RepublishAll registered %d workflows, want 1 (bad one skipped)", n)
	}
}

// TestRepublishAll_NilEngineNoop verifies that a store-only service (nil
// engine) returns 0 without error — mirrors Publish/Update's nil-engine guard.
func TestRepublishAll_NilEngineNoop(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, nil)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n, err := svc.RepublishAll(ctx)
	if err != nil {
		t.Fatalf("RepublishAll (nil engine): %v", err)
	}
	if n != 0 {
		t.Fatalf("RepublishAll with nil engine returned %d, want 0", n)
	}
}

func TestUpdate_RecompilesAndReregisters(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Original",
		TriggerIdentifier: "upd-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "upd-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Update to a two-step workflow.
	updated, err := svc.Update(ctx, scope, "upd-wf", authoring.UpdateInput{
		Name: "Updated",
		Steps: []domain.Step{
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"v2"}`)},
			{StepID: "S2", Type: "email", ControlValues: json.RawMessage(`{"subject":"s","body":"b"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "Updated" {
		t.Fatalf("updated.Name = %q, want Updated", updated.Name)
	}
	// TriggerIdentifier must not change.
	if updated.TriggerIdentifier != "upd-wf" {
		t.Fatalf("TriggerIdentifier changed to %q", updated.TriggerIdentifier)
	}
}

func TestUpdate_ReturnsNotFoundForMissingWorkflow(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	_, err = svc.Update(ctx, scope, "no-such-slug", authoring.UpdateInput{
		Name:  "X",
		Steps: []domain.Step{{StepID: "S1", Type: "in_app"}},
	})
	if err == nil {
		t.Fatal("expected error for missing workflow")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound, got %v", err)
	}
}

func TestUpdate_422OnBadDelayStep(t *testing.T) {
	// Compile-failure test is pure (no engine needed): the bad step is caught
	// before any store write occurs, so we can use a nil engine stand-in that
	// is never called.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "WF",
		TriggerIdentifier: "bad-delay",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "bad-delay"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Attempt to update with a delay step missing a positive amount.
	_, err = svc.Update(ctx, scope, "bad-delay", authoring.UpdateInput{
		Name: "Bad",
		Steps: []domain.Step{
			{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"amount":0,"unit":"seconds"}`)},
		},
	})
	if err == nil {
		t.Fatal("expected ErrCompileFailure for bad delay step")
	}
	var compileErr *authoring.ErrCompileFailure
	if !errors.As(err, &compileErr) {
		t.Fatalf("expected *authoring.ErrCompileFailure, got %T: %v", err, err)
	}
}

func TestDelete_RemovesWorkflow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Bye",
		TriggerIdentifier: "del-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "del-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if err := svc.Delete(ctx, scope, "del-wf"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// GetBySlug must return ErrNotFound.
	if _, err := svc.GetBySlug(ctx, scope, "del-wf"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}

	// Deleting again must return ErrNotFound.
	if err := svc.Delete(ctx, scope, "del-wf"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for double-delete, got %v", err)
	}
}

func TestDuplicate_CopiesStepsAndPublishes(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	origSteps := []domain.Step{
		{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hello"}`)},
	}
	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Source",
		TriggerIdentifier: "src-wf",
		Steps:             origSteps,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "src-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	dup, err := svc.Duplicate(ctx, scope, "src-wf", authoring.DuplicateInput{
		Name:              "Source (Copy)",
		TriggerIdentifier: "src-wf-copy",
	})
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if dup.TriggerIdentifier != "src-wf-copy" {
		t.Fatalf("TriggerIdentifier = %q, want src-wf-copy", dup.TriggerIdentifier)
	}
	if dup.Name != "Source (Copy)" {
		t.Fatalf("Name = %q, want 'Source (Copy)'", dup.Name)
	}

	// Source steps must be inherited.
	src, err := svc.GetBySlug(ctx, scope, "src-wf")
	if err != nil {
		t.Fatalf("GetBySlug src: %v", err)
	}
	if string(dup.Steps) != string(src.Steps) {
		t.Fatalf("steps mismatch: dup=%s src=%s", dup.Steps, src.Steps)
	}
}

func TestDuplicate_409OnIdentifierCollision(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	for _, slug := range []string{"dup-src", "dup-existing"} {
		if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
			Name:              slug,
			TriggerIdentifier: slug,
			Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
		}); err != nil {
			t.Fatalf("Create %s: %v", slug, err)
		}
		if err := svc.Publish(ctx, scope, slug); err != nil {
			t.Fatalf("Publish %s: %v", slug, err)
		}
	}

	_, err = svc.Duplicate(ctx, scope, "dup-src", authoring.DuplicateInput{
		Name:              "Collision",
		TriggerIdentifier: "dup-existing",
	})
	if err == nil {
		t.Fatal("expected ErrIdentifierConflict for duplicate identifier")
	}
	if !errors.Is(err, authoring.ErrIdentifierConflict) {
		t.Fatalf("expected ErrIdentifierConflict, got %T: %v", err, err)
	}
}

// TestDuplicate_PreservesCritical is the no-opt-out guarantee under cloning:
// duplicating a critical workflow (e.g. "password-reset") must yield a critical
// copy — losing it silently on the new row would strip the exact protection the
// flag exists to provide. The paired non-critical case guards against a
// copy that spuriously flips criticality on. Uses a nil engine (Duplicate's
// Publish is a no-op without one), so no sidecar is needed.
func TestDuplicate_PreservesCritical(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	svc := authoring.New(st, nil)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	steps := []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"reset"}`)}}

	// Critical source -> critical copy.
	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Password Reset",
		TriggerIdentifier: "password-reset",
		Steps:             steps,
		Critical:          true,
	}); err != nil {
		t.Fatalf("Create critical: %v", err)
	}
	dup, err := svc.Duplicate(ctx, scope, "password-reset", authoring.DuplicateInput{
		Name:              "Password Reset (Copy)",
		TriggerIdentifier: "password-reset-copy",
	})
	if err != nil {
		t.Fatalf("Duplicate critical: %v", err)
	}
	if !dup.Critical {
		t.Fatalf("duplicate of a critical workflow lost Critical: got %+v", dup)
	}
	// Persisted row must also be critical (not just the returned value).
	got, err := svc.GetBySlug(ctx, scope, "password-reset-copy")
	if err != nil {
		t.Fatalf("GetBySlug copy: %v", err)
	}
	if !got.Critical {
		t.Fatalf("persisted duplicate is not critical: got %+v", got)
	}

	// Non-critical source -> non-critical copy (guard against a flip-on).
	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             steps,
	}); err != nil {
		t.Fatalf("Create non-critical: %v", err)
	}
	plainDup, err := svc.Duplicate(ctx, scope, "welcome", authoring.DuplicateInput{
		Name:              "Welcome (Copy)",
		TriggerIdentifier: "welcome-copy",
	})
	if err != nil {
		t.Fatalf("Duplicate non-critical: %v", err)
	}
	if plainDup.Critical {
		t.Fatalf("duplicate of a non-critical workflow became critical: got %+v", plainDup)
	}
}

func TestSetActive_TogglesActiveFlag(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	natsURL, bridgeURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Stop()

	svc := authoring.New(st, eng)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	if _, err := svc.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Toggle",
		TriggerIdentifier: "toggle-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Publish(ctx, scope, "toggle-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	wf, err := svc.SetActive(ctx, scope, "toggle-wf", false)
	if err != nil {
		t.Fatalf("SetActive false: %v", err)
	}
	if wf.Active {
		t.Fatal("expected active=false after SetActive(false)")
	}

	wf, err = svc.SetActive(ctx, scope, "toggle-wf", true)
	if err != nil {
		t.Fatalf("SetActive true: %v", err)
	}
	if !wf.Active {
		t.Fatal("expected active=true after SetActive(true)")
	}
}

// TestCompile_ThrottleStep_Valid verifies a throttle step compiles to the
// check.throttle normal task (guard: must match the eng.Handle key in main.go).
func TestCompile_ThrottleStep_Valid(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "throttle-test",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "T1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":5,"windowSec":60}`)},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(def.Steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(def.Steps))
	}
	s := def.Steps[0]
	if s.Task != "check.throttle" {
		t.Errorf("Task = %q, want check.throttle", s.Task)
	}
	if s.Type != "normal" {
		t.Errorf("Type = %q, want normal", s.Type)
	}
	if s.ID != "T1" {
		t.Errorf("ID = %q, want T1", s.ID)
	}
	if len(s.DependsOn) != 0 {
		t.Errorf("root throttle DependsOn = %v, want empty", s.DependsOn)
	}
}

// TestCompile_ThrottleStep_ZeroLimit verifies that limit=0 produces a compile
// error, mapping to HTTP 422 when submitted via the console.
func TestCompile_ThrottleStep_ZeroLimit(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "throttle-zero-limit",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "T1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":0,"windowSec":60}`)},
		}),
	}
	_, err := authoring.Compile(wf)
	if err == nil {
		t.Fatal("Compile: expected error for limit=0, got nil")
	}
}

// TestCompile_ThrottleStep_ZeroWindow verifies that windowSec=0 produces a
// compile error.
func TestCompile_ThrottleStep_ZeroWindow(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "throttle-zero-window",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "T1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":1,"windowSec":0}`)},
		}),
	}
	_, err := authoring.Compile(wf)
	if err == nil {
		t.Fatal("Compile: expected error for windowSec=0, got nil")
	}
}

// TestCompile_ThrottleBeforeChannel verifies a [throttle, in_app] workflow
// compiles to [check.throttle (no DependsOn), send.in_app (DependsOn=[T1])].
func TestCompile_ThrottleBeforeChannel(t *testing.T) {
	wf := domain.Workflow{
		EnvID:             "env_1",
		TriggerIdentifier: "throttle-then-channel",
		Steps: mustSteps(t, []domain.Step{
			{StepID: "T1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":1,"windowSec":3600}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"hi","body":"there"}`)},
		}),
	}
	def, err := authoring.Compile(wf)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(def.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(def.Steps))
	}
	thr := def.Steps[0]
	if thr.Task != "check.throttle" || thr.Type != "normal" {
		t.Errorf("throttle step = %+v, want {Task:check.throttle Type:normal}", thr)
	}
	if len(thr.DependsOn) != 0 {
		t.Errorf("throttle DependsOn = %v, want empty", thr.DependsOn)
	}
	ch := def.Steps[1]
	if ch.Task != "send.in_app" || ch.Type != "normal" {
		t.Errorf("channel step = %+v, want {Task:send.in_app Type:normal}", ch)
	}
	if len(ch.DependsOn) != 1 || ch.DependsOn[0] != "T1" {
		t.Errorf("channel DependsOn = %v, want [T1]", ch.DependsOn)
	}
}
