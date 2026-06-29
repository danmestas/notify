package authoring_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
)

// updateGolden regenerates the golden files instead of comparing against them.
// Run with: go test ./internal/authoring -run Golden -update
var updateGolden = flag.Bool("update", false, "update golden files")

// goldenCompileCase is one representative workflow for a single step type. The
// golden captures the exact compiled engine.WorkflowDef JSON so the step-type
// registry refactor can be proven byte-identical (NO behavior change).
type goldenCompileCase struct {
	name  string
	steps []domain.Step
}

// goldenCompileCases covers all 7 migrated step types, one single-step
// workflow each. Single-step is sufficient: DependsOn chaining is
// type-independent; what changes per type is the StepDef Task/Type/Config.
var goldenCompileCases = []goldenCompileCase{
	{"in_app", []domain.Step{{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"Welcome {{payload.name}}"}`)}}},
	{"email", []domain.Step{{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Your receipt","body":"Total: {{payload.total}}"}`)}}},
	{"push", []domain.Step{{StepID: "push-1", Type: "push", ControlValues: json.RawMessage(`{"title":"Ping","body":"Push body"}`)}}},
	{"sms", []domain.Step{{StepID: "sms-1", Type: "sms", ControlValues: json.RawMessage(`{"body":"SMS body"}`)}}},
	{"chat", []domain.Step{{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"Chat body"}`)}}},
	{"delay", []domain.Step{{StepID: "delay-1", Type: "delay", ControlValues: json.RawMessage(`{"amount":5,"unit":"hours"}`)}}},
	{"digest", []domain.Step{{StepID: "digest-1", Type: "digest", ControlValues: json.RawMessage(`{"amount":2,"unit":"days"}`)}}},
}

// TestCompile_Golden pins authoring.Compile output for every step type. It is
// the no-behavior-change gate for the step-type registry refactor: any diff in
// the compiled engine DAG JSON is a behavior change to fix.
func TestCompile_Golden(t *testing.T) {
	for _, tc := range goldenCompileCases {
		t.Run(tc.name, func(t *testing.T) {
			rawSteps, err := json.Marshal(tc.steps)
			if err != nil {
				t.Fatalf("marshal steps: %v", err)
			}
			wf := domain.Workflow{
				EnvID:             "env_golden",
				TriggerIdentifier: "wf-" + tc.name,
				Steps:             rawSteps,
			}
			def, err := authoring.Compile(wf)
			if err != nil {
				t.Fatalf("Compile(%s): %v", tc.name, err)
			}
			got, err := json.MarshalIndent(def, "", "  ")
			if err != nil {
				t.Fatalf("marshal def: %v", err)
			}
			got = append(got, '\n')
			assertGolden(t, filepath.Join("testdata", "compile_"+tc.name+".json.golden"), got)
		})
	}
}

// assertGolden compares got against the golden file at path, or rewrites the
// file when -update is set / the file is missing (first-run baseline capture).
func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(path)
	if *updateGolden || os.IsNotExist(err) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			t.Fatalf("mkdir golden dir: %v", mkErr)
		}
		if wErr := os.WriteFile(path, got, 0o644); wErr != nil {
			t.Fatalf("write golden %s: %v", path, wErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
