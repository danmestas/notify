package steptype_test

import (
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestHandlerDesyncGuard asserts that the set of "normal" engine tasks emitted
// by steptype.Default() is exactly expectedNormalTasks. It catches two failure
// modes:
//
//  1. A new step type is added to Default() that compiles to a "normal" task but
//     is not in expectedNormalTasks → the loop over gotNormal fails.
//  2. An entry is removed from expectedNormalTasks that still appears in
//     gotNormal → the symmetry check fails.
//
// IMPORTANT: expectedNormalTasks must mirror the eng.Handle registrations in
// cmd/notify/main.go. Adding a task here without adding a matching
// eng.Handle(task, ...) in main.go means the task will never be dispatched.
// The engine's Handles() smoke test in internal/httpapi/throttle_e2e_test.go
// catches the reverse — a task registered in main.go but not covered here.
func TestHandlerDesyncGuard(t *testing.T) {
	// expectedNormalTasks is the complete set of Type:"normal" engine task names
	// that steptype.Default() can produce at compile time. Every entry must have
	// a matching eng.Handle call in cmd/notify/main.go.
	expectedNormalTasks := map[string]bool{
		"send.in_app":     true,
		"send.email":      true,
		"send.push":       true,
		"send.sms":        true,
		"send.chat":       true,
		"digest.wait":     true,
		"check.throttle":  true,
		"http_request":    true,
		"delay.scheduled": true,
		"delay.dynamic":   true,
	}

	// representativeSteps covers each step type and each variant that can
	// produce a "normal" StepDef. Regular delay/digest compile to a sleep (not
	// normal), so only the non-sleep variants are listed.
	representativeSteps := []domain.Step{
		{StepID: "s", Type: "in_app"},
		{StepID: "s", Type: "email"},
		{StepID: "s", Type: "push"},
		{StepID: "s", Type: "sms"},
		{StepID: "s", Type: "chat"},
		{
			StepID: "s", Type: "digest",
			ControlValues: json.RawMessage(`{"type":"scheduled","cron":"0 * * * *"}`),
		},
		{
			StepID: "s", Type: "throttle",
			ControlValues: json.RawMessage(`{"limit":1,"windowSec":3600}`),
		},
		{
			StepID: "s", Type: "http_request",
			ControlValues: json.RawMessage(`{"url":"http://example.com","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`),
		},
		{
			StepID: "s", Type: "delay",
			ControlValues: json.RawMessage(`{"type":"scheduled","atTime":"09:00","weekDays":["MON"],"monthDays":[1]}`),
		},
		{
			StepID: "s", Type: "delay",
			ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.waitUntil"}`),
		},
	}

	reg := steptype.Default()
	gotNormal := map[string]bool{}
	for _, step := range representativeSteps {
		st := reg.Get(step.Type)
		if st == nil {
			t.Errorf("step type %q not found in Default() registry", step.Type)
			continue
		}
		def, err := st.Compile(step)
		if err != nil {
			t.Errorf("Compile(%q cv=%s): %v", step.Type, step.ControlValues, err)
			continue
		}
		if def.Type == "normal" {
			gotNormal[def.Task] = true
		}
	}

	// gotNormal ⊆ expectedNormalTasks: any new "normal" task must be added to
	// expectedNormalTasks and to cmd/notify/main.go's eng.Handle calls.
	for task := range gotNormal {
		if !expectedNormalTasks[task] {
			t.Errorf("step type compiles to normal task %q not in expectedNormalTasks — "+
				"add eng.Handle(%q, ...) to cmd/notify/main.go and add it to expectedNormalTasks here", task, task)
		}
	}

	// expectedNormalTasks ⊆ gotNormal: if an entry was removed from the registry
	// but kept in the expected set, the expected set must be trimmed too.
	for task := range expectedNormalTasks {
		if !gotNormal[task] {
			t.Errorf("expectedNormalTasks includes %q but no representative step compiled to it — "+
				"update representativeSteps or remove the entry from expectedNormalTasks", task)
		}
	}
}
