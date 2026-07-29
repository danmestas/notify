package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// seedWorkflowWithSteps writes a workflow row directly, bypassing authoring
// validation — which is the point: it simulates a workflow saved BEFORE the
// contract tightened, exactly what this command exists to find.
func seedWorkflowWithSteps(t *testing.T, dbPath, trigger string, steps []domain.Step) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	raw, err := json.Marshal(steps)
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := st.Workflows().Create(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, domain.Workflow{
		Name: trigger, TriggerIdentifier: trigger, Active: true, Steps: raw,
	}); err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}
}

// TestValidateSteps_PassesOnConformingWorkflows proves the command is quiet when
// everything conforms — the expected outcome on a healthy database.
func TestValidateSteps_PassesOnConformingWorkflows(t *testing.T) {
	db := filepath.Join(t.TempDir(), "notify.db")
	seedWorkflowWithSteps(t, db, "good-wf", []domain.Step{
		{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.sendAt","dynamicKind":"instant"}`)},
		{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"0 9 * * MON"}`)},
	})

	if err := runValidateSteps([]string{"-db", db}); err != nil {
		t.Fatalf("conforming workflows reported as failures: %v", err)
	}
}

// TestValidateSteps_FindsDynamicDelayWithoutKind is the #258 case: a dynamic
// delay saved before dynamicKind was required.
func TestValidateSteps_FindsDynamicDelayWithoutKind(t *testing.T) {
	db := filepath.Join(t.TempDir(), "notify.db")
	seedWorkflowWithSteps(t, db, "legacy-dynamic", []domain.Step{
		{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.sendAt"}`)},
	})

	err := runValidateSteps([]string{"-db", db})
	if err == nil {
		t.Fatal("a dynamic delay with no dynamicKind was not reported")
	}
}

// TestValidateSteps_FindsLooseDigestCron is the other #258 case: a digest saved
// with a 6-field or descriptor cron the engine cannot express.
func TestValidateSteps_FindsLooseDigestCron(t *testing.T) {
	for _, spec := range []string{`{"type":"scheduled","cron":"@hourly"}`, `{"type":"scheduled","cron":"* * * * * *"}`} {
		db := filepath.Join(t.TempDir(), "notify.db")
		seedWorkflowWithSteps(t, db, "legacy-digest", []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(spec)},
		})

		if err := runValidateSteps([]string{"-db", db}); err == nil {
			t.Errorf("loose cron %s was not reported", spec)
		}
	}
}

// TestValidateSteps_EmptyDatabase guards the expected real-world outcome: with
// no workflows stored, the command succeeds rather than erroring on nothing.
func TestValidateSteps_EmptyDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "notify.db")
	st, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = st.Close()

	if err := runValidateSteps([]string{"-db", db}); err != nil {
		t.Fatalf("empty database reported a failure: %v", err)
	}
}
