package authoring_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/names"
)

// legacyDef is a compiled_def as stored before the notify. prefix (N4).
const legacyDef = `{"Name":"env_1-welcome","Steps":[` +
	`{"ID":"S1","Task":"send.in_app","Type":"normal"},` +
	`{"ID":"S2","Task":"delay.scheduled","Type":"normal","DependsOn":["S1"]}]}`

// TestRepublishAll_UpgradesPrePrefixDefs: a version stored before the prefix
// existed is registered under the names trigger now starts and the workers
// handle, without touching the stored bytes.
func TestRepublishAll_UpgradesPrePrefixDefs(t *testing.T) {
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
	if _, err := st.WorkflowVersions().Create(ctx, scope, wf.ID, "Welcome", "welcome",
		json.RawMessage(legacyDef), string(wf.Steps), string(wf.PayloadSchema), false); err != nil {
		t.Fatalf("seed legacy version: %v", err)
	}

	if _, err := svc.RepublishAll(ctx); err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	got := fake.last()
	if got.Name != names.Workflow("env_1", "welcome") {
		t.Errorf("workflow name = %q, want %q", got.Name, names.Workflow("env_1", "welcome"))
	}
	wantTasks := []string{"notify.send.in_app", "notify.delay.scheduled"}
	for i, s := range got.Steps {
		if s.Task != wantTasks[i] {
			t.Errorf("step %s task = %q, want %q", s.ID, s.Task, wantTasks[i])
		}
	}

	ver, err := st.WorkflowVersions().LatestByWorkflow(ctx, wf.ID)
	if err != nil {
		t.Fatalf("LatestByWorkflow: %v", err)
	}
	if string(ver.CompiledDef) != legacyDef {
		t.Errorf("stored compiled_def was rewritten: %s", ver.CompiledDef)
	}
}

// TestCompile_EmitsOnlyPrefixedNames pins what authoring builds today.
func TestCompile_EmitsOnlyPrefixedNames(t *testing.T) {
	steps, err := json.Marshal([]domain.Step{
		{StepID: "S1", Type: "email", ControlValues: json.RawMessage(`{}`)},
		{StepID: "S2", Type: "no_such_type"},
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err := authoring.Compile(domain.Workflow{EnvID: "env_1", TriggerIdentifier: "x", Steps: steps})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.Name != "notify.env_1-x" {
		t.Errorf("name = %q", def.Name)
	}
	for _, s := range def.Steps {
		if len(s.Task) < 7 || s.Task[:7] != "notify." {
			t.Errorf("step %s task %q lacks the notify. prefix", s.ID, s.Task)
		}
	}
}
