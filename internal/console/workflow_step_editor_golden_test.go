package console

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// updateStepEditorGolden regenerates the step-editor golden HTML instead of
// comparing. Run: go test ./internal/console -run StepEditorGolden -update
var updateStepEditorGolden = flag.Bool("update", false, "update golden files")

// stepEditorGoldenCase is one representative step for a single type.
type stepEditorGoldenCase struct {
	name string
	step domain.Step
}

// stepEditorGoldenCases covers all 8 migrated step types. The golden captures
// the byte-exact rendered "workflow-step-editor" fragment so the step-type
// registry refactor (which moves per-type field markup behind EditorPartial)
// can be proven byte-identical (NO behavior change).
var stepEditorGoldenCases = []stepEditorGoldenCase{
	{"in_app", domain.Step{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"In-app subject","body":"In-app body {{payload.x}}"}`)}},
	{"email", domain.Step{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Email subject","body":"Email body {{payload.x}}"}`)}},
	{"push", domain.Step{StepID: "push-1", Type: "push", ControlValues: json.RawMessage(`{"title":"Push title","body":"Push body {{payload.x}}"}`)}},
	{"sms", domain.Step{StepID: "sms-1", Type: "sms", ControlValues: json.RawMessage(`{"body":"SMS body {{payload.x}}"}`)}},
	{"chat", domain.Step{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"Chat body {{payload.x}}"}`)}},
	{"delay", domain.Step{StepID: "delay-1", Type: "delay", ControlValues: json.RawMessage(`{"amount":5,"unit":"hours"}`)}},
	{"digest", domain.Step{StepID: "digest-1", Type: "digest", ControlValues: json.RawMessage(`{"amount":2,"unit":"days"}`)}},
	{"throttle", domain.Step{StepID: "throttle-1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":5,"windowSec":3600}`)}},
}

// TestStepEditorGolden pins the rendered per-type step-editor HTML. It mirrors
// serveFragmentWorkflowStepEditor's view construction exactly so the golden is
// faithful to the production render path.
func TestStepEditorGolden(t *testing.T) {
	tc := newTestConsole(t)
	for _, gc := range stepEditorGoldenCases {
		t.Run(gc.name, func(t *testing.T) {
			combinator, conditions, visible := conditionsFromFilters(gc.step.Filters)
			// Mirror serveFragmentWorkflowStepEditor's registry-driven view
			// construction so the golden is faithful to the production path.
			stype := tc.c.stepTypes.Get(gc.step.Type)
			view := workflowStepEditorView{
				Slug:     "wf-golden",
				CSRF:     "csrf-golden",
				ReadOnly: false,
				Step: &workflowStepEditorStep{
					StepID:            gc.step.StepID,
					Type:              gc.step.Type,
					Name:              gc.step.Name,
					ControlValuesJSON: prettyJSON(gc.step.ControlValues),
					ControlMap:        stype.ControlMap(gc.step.ControlValues),
					EditorPartial:     stype.EditorPartial(),
					Combinator:        combinator,
					Conditions:        conditions,
					VisibleConditions: visible,
				},
			}
			html, err := renderFragment(tc.c.ts.base, "workflow-step-editor", view)
			if err != nil {
				t.Fatalf("render step editor (%s): %v", gc.name, err)
			}
			assertStepEditorGolden(t, filepath.Join("testdata", "step_editor_"+gc.name+".html.golden"), []byte(html))
		})
	}
}

func assertStepEditorGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(path)
	if *updateStepEditorGolden || os.IsNotExist(err) {
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
