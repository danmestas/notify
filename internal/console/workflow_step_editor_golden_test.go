package console

import (
	"bytes"
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
	// fullFragment keeps the ENTIRE rendered fragment, including the shared
	// conditions block. Exactly one case sets it; the rest are slimmed to their
	// type-specific field region. See conditionsMarker.
	fullFragment bool
}

// stepEditorGoldenCases covers all 8 migrated step types. The golden captures
// the byte-exact rendered "workflow-step-editor" fragment so the step-type
// registry refactor (which moves per-type field markup behind EditorPartial)
// can be proven byte-identical (NO behavior change).
var stepEditorGoldenCases = []stepEditorGoldenCase{
	// in_app is the anchor: it keeps the whole fragment so the shared chassis
	// around every editor (signals, conditions fieldset, action row) stays
	// pinned exactly once.
	{"in_app", domain.Step{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"In-app subject","body":"In-app body {{payload.x}}"}`)}, true},
	{"email", domain.Step{StepID: "email-1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Email subject","body":"Email body {{payload.x}}"}`)}, false},
	{"push", domain.Step{StepID: "push-1", Type: "push", ControlValues: json.RawMessage(`{"title":"Push title","body":"Push body {{payload.x}}"}`)}, false},
	{"sms", domain.Step{StepID: "sms-1", Type: "sms", ControlValues: json.RawMessage(`{"body":"SMS body {{payload.x}}"}`)}, false},
	{"chat", domain.Step{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"Chat body {{payload.x}}"}`)}, false},
	{"delay", domain.Step{StepID: "delay-1", Type: "delay", ControlValues: json.RawMessage(`{"amount":5,"unit":"hours"}`)}, false},
	{"digest", domain.Step{StepID: "digest-1", Type: "digest", ControlValues: json.RawMessage(`{"amount":2,"unit":"days"}`)}, false},
	{"throttle", domain.Step{StepID: "throttle-1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":5,"windowSec":3600}`)}, false},
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
			body := []byte(html)
			if !gc.fullFragment {
				body = fieldRegion(body)
			}
			assertStepEditorGolden(t, filepath.Join("testdata", "step_editor_"+gc.name+".html.golden"), body)
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

// conditionsMarker opens the shared "when should this step run?" fieldset, which
// is identical in every step editor and made up the overwhelming majority of
// each golden -- the same block was stored eight times over.
const conditionsMarker = `<fieldset class="wf-conditions">`

// fieldRegion trims a rendered editor fragment to the part that actually varies
// by step type: everything before the shared conditions fieldset.
//
// The shared region is not untested -- the in_app case pins it in full. Storing
// it eight times bought nothing except a diff nobody can read when the shared
// chassis changes, because all eight goldens churn at once.
func fieldRegion(html []byte) []byte {
	if i := bytes.Index(html, []byte(conditionsMarker)); i >= 0 {
		return html[:i]
	}
	// Marker missing means the shared chassis was renamed or removed. Returning
	// the whole fragment makes that show up as a large, obvious golden diff
	// rather than silently slimming to nothing.
	return html
}
