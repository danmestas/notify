package authoring_test

import (
	"context"
	"encoding/json"
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
