package notify

import (
	"sort"
	"testing"

	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
)

// recordingRegistrar captures the task types the real wiring registers. Using
// the actual registration function -- rather than a second list restating what
// it is believed to do -- is the point: a duplicated list can drift from the
// code exactly as easily as the code can drift from the registry.
type recordingRegistrar struct{ registered []string }

func (r *recordingRegistrar) Handle(taskType string, _ engine.HandlerFunc, _ ...engine.HandleOption) {
	r.registered = append(r.registered, taskType)
}

// TestWiring_CoversEveryRegistryTaskType is the handler-desync guard from #164.
//
// A step type that compiles to a normal engine task needs a handler registered
// in the worker. If one is missing, nothing fails at boot: the workflow
// publishes, the trigger is accepted, the task is enqueued -- and then sits
// there forever, because no worker polls that task type. The failure surfaces
// only as "the notification never arrived".
//
// The invariant used to live in comments at the registration sites ("must be
// registered whenever steptype.Default() includes X"). This asserts it.
func TestWiring_CoversEveryRegistryTaskType(t *testing.T) {
	rec := &recordingRegistrar{}
	// Nil dependencies are fine: registration only constructs handlers and
	// stores the func values; nothing dials or reads until a task dispatches.
	registerStepTaskHandlers(rec, stepTaskDeps{})

	got := map[string]bool{}
	for _, name := range rec.registered {
		got[name] = true
	}

	var missing []string
	for _, want := range steptype.Default().TaskTypes() {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("step types compile to task(s) with no registered handler: %v\n"+
			"A task with no handler is enqueued and never polled: the workflow "+
			"publishes, the trigger is accepted, and the notification silently "+
			"never arrives. Register a handler in registerStepTaskHandlers.", missing)
	}
}

// TestWiring_RegistersNoUnknownTaskType catches the opposite drift: a handler
// wired for a task type no step type can produce. That is dead wiring -- a
// worker polling a queue nothing writes to -- and usually means a task was
// renamed on one side only.
//
// Task types that are not step-derived (the credential-verify and agent-mesh
// actions) are registered elsewhere in boot and are deliberately out of scope.
func TestWiring_RegistersNoUnknownTaskType(t *testing.T) {
	rec := &recordingRegistrar{}
	registerStepTaskHandlers(rec, stepTaskDeps{})

	known := map[string]bool{}
	for _, name := range steptype.Default().TaskTypes() {
		known[name] = true
	}

	var orphaned []string
	for _, name := range rec.registered {
		if !known[name] {
			orphaned = append(orphaned, name)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		t.Errorf("handler(s) registered for task type(s) no step type compiles to: %v\n"+
			"This is dead wiring; check for a rename applied on only one side.", orphaned)
	}
}

// TestRegistryTaskTypes_Deduped pins the registry helper itself: channel step
// types each contribute one send.* name, and the set is sorted and unique so
// callers can compare it directly.
func TestRegistryTaskTypes_Deduped(t *testing.T) {
	got := steptype.Default().TaskTypes()

	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Errorf("TaskTypes returned %q twice", name)
		}
		seen[name] = true
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("TaskTypes is not sorted: %v", got)
	}
	for _, want := range []string{"notify.send.in_app", "notify.delay.dynamic", "notify.digest.wait", "notify.check.throttle", "notify.http_request"} {
		if !seen[want] {
			t.Errorf("TaskTypes missing %q; got %v", want, got)
		}
	}
}
