package aibuilder

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/delivery/filter"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// counter returns a deterministic StepID generator for tests (s1, s2, ...).
func counter() idGen {
	i := 0
	return func() string {
		i++
		return fmt.Sprintf("s%d", i)
	}
}

func reg() *steptype.Registry { return steptype.Default() }

func mustApply(t *testing.T, d *WorkflowDraft, next idGen, tool, args string) Event {
	t.Helper()
	ev, _, err := applyToolCall(d, reg(), next, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("applyToolCall(%s) unexpected error: %v", tool, err)
	}
	return ev
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Welcome Email!":    "welcome-email",
		"Order   Shipped":   "order-shipped",
		"  Trim Me  ":       "trim-me",
		"Already-Slug":      "already-slug",
		"Caps AND Symbols$": "caps-and-symbols",
		"a:b":               "a-b", // colon must never survive (NATS KV key safety)
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetWorkflowMetadata_SetsNameAndSlugsTrigger(t *testing.T) {
	d := &WorkflowDraft{}
	ev := mustApply(t, d, counter(), toolSetWorkflowMetadata, `{"name":"Welcome Email!"}`)
	if ev.Kind != KindCanvas {
		t.Errorf("kind = %q, want canvas", ev.Kind)
	}
	if d.Name != "Welcome Email!" {
		t.Errorf("Name = %q", d.Name)
	}
	if d.TriggerIdentifier != "welcome-email" {
		t.Errorf("TriggerIdentifier = %q, want welcome-email", d.TriggerIdentifier)
	}
}

func TestSetWorkflowMetadata_PreservesExistingTrigger(t *testing.T) {
	d := &WorkflowDraft{TriggerIdentifier: "locked-id"}
	mustApply(t, d, counter(), toolSetWorkflowMetadata, `{"name":"New Name"}`)
	if d.TriggerIdentifier != "locked-id" {
		t.Errorf("TriggerIdentifier = %q, want locked-id (immutable once set)", d.TriggerIdentifier)
	}
}

func TestSetWorkflowMetadata_RequiresName(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolSetWorkflowMetadata, json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestAddStep_AppendsWithDefaultControls(t *testing.T) {
	d := &WorkflowDraft{}
	ev := mustApply(t, d, counter(), toolAddStep, `{"type":"email","name":"Greeting"}`)
	if ev.Kind != KindCanvas {
		t.Errorf("kind = %q, want canvas", ev.Kind)
	}
	if len(d.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(d.Steps))
	}
	s := d.Steps[0]
	if s.StepID != "s1" {
		t.Errorf("StepID = %q, want s1", s.StepID)
	}
	if s.Type != "email" {
		t.Errorf("Type = %q, want email", s.Type)
	}
	if s.Name != "Greeting" {
		t.Errorf("Name = %q, want Greeting", s.Name)
	}
	want := string(steptype.Default().Get("email").DefaultControlValues())
	if string(s.ControlValues) != want {
		t.Errorf("ControlValues = %s, want %s", s.ControlValues, want)
	}
}

func TestAddStep_UnknownTypeErrors(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolAddStep, json.RawMessage(`{"type":"telepathy"}`)); err == nil {
		t.Fatal("expected error for unknown step type")
	}
	if len(d.Steps) != 0 {
		t.Errorf("draft mutated on error: %d steps", len(d.Steps))
	}
}

func TestAddStepInBetween_SplicesAfterTarget(t *testing.T) {
	d := &WorkflowDraft{}
	next := counter()
	mustApply(t, d, next, toolAddStep, `{"type":"email"}`) // s1
	mustApply(t, d, next, toolAddStep, `{"type":"sms"}`)   // s2
	mustApply(t, d, next, toolAddStepInBetween,            //
		`{"type":"delay","afterStepId":"s1"}`) // s3 between s1 and s2
	got := []string{d.Steps[0].StepID, d.Steps[1].StepID, d.Steps[2].StepID}
	want := []string{"s1", "s3", "s2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if d.Steps[1].Type != "delay" {
		t.Errorf("inserted type = %q, want delay", d.Steps[1].Type)
	}
}

func TestAddStepInBetween_UnknownAfterErrors(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolAddStepInBetween,
		json.RawMessage(`{"type":"email","afterStepId":"nope"}`)); err == nil {
		t.Fatal("expected error for unknown afterStepId")
	}
}

func TestEditStepContent_SetsControlValues(t *testing.T) {
	d := &WorkflowDraft{}
	next := counter()
	mustApply(t, d, next, toolAddStep, `{"type":"email"}`)
	mustApply(t, d, next, toolEditStepContent,
		`{"stepId":"s1","controlValues":{"subject":"Hi","body":"Hello there"}}`)
	var cv map[string]string
	if err := json.Unmarshal(d.Steps[0].ControlValues, &cv); err != nil {
		t.Fatal(err)
	}
	if cv["subject"] != "Hi" || cv["body"] != "Hello there" {
		t.Errorf("controlValues = %v", cv)
	}
}

func TestEditStepContent_UnknownStepErrors(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolEditStepContent,
		json.RawMessage(`{"stepId":"ghost","controlValues":{}}`)); err == nil {
		t.Fatal("expected error for unknown stepId")
	}
}

func TestEditStepContent_RejectsNonObjectControlValues(t *testing.T) {
	for _, cv := range []string{`5`, `"hi"`, `[1,2,3]`, `true`, `null`} {
		d := &WorkflowDraft{}
		next := counter()
		mustApply(t, d, next, toolAddStep, `{"type":"email"}`)
		args := fmt.Sprintf(`{"stepId":"s1","controlValues":%s}`, cv)
		if _, _, err := applyToolCall(d, reg(), next, toolEditStepContent, json.RawMessage(args)); err == nil {
			t.Errorf("controlValues=%s: expected error for non-object controlValues", cv)
		}
		// The rejected edit must not have mutated the step's control values.
		if string(d.Steps[0].ControlValues) != string(steptype.Default().Get("email").DefaultControlValues()) {
			t.Errorf("controlValues=%s: step mutated despite rejection: %s", cv, d.Steps[0].ControlValues)
		}
	}
}

// TestEditStepContent_FrontLoadsStepTypeValidate proves editStepContent calls
// the target step's steptype.Validate hook BEFORE mutating the draft: a
// controlValues blob that is a well-formed JSON object (passes isJSONObject)
// but fails the step type's own validation (an http_request method outside
// GET/POST/PUT/PATCH/DELETE) is rejected with an error that mentions the
// offending field, and the step's control values are left untouched.
func TestEditStepContent_FrontLoadsStepTypeValidate(t *testing.T) {
	d := &WorkflowDraft{}
	next := counter()
	mustApply(t, d, next, toolAddStep, `{"type":"http_request"}`) // s1
	before := string(d.Steps[0].ControlValues)

	args := `{"stepId":"s1","controlValues":{"url":"https://example.com","method":"WOOF","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}}`
	_, _, err := applyToolCall(d, reg(), next, toolEditStepContent, json.RawMessage(args))
	if err == nil {
		t.Fatal("expected error for an http_request method outside GET/POST/PUT/PATCH/DELETE")
	}
	if !strings.Contains(err.Error(), "method") {
		t.Errorf("error = %v, want it to mention the offending field (method)", err)
	}
	if string(d.Steps[0].ControlValues) != before {
		t.Errorf("step mutated despite rejected edit: got %s, want unchanged %s", d.Steps[0].ControlValues, before)
	}
}

// TestEditStepContent_AcceptsValidStepTypeControlValues is the positive
// counterpart: a well-formed edit that also passes the step type's Validate
// hook is applied normally.
func TestEditStepContent_AcceptsValidStepTypeControlValues(t *testing.T) {
	d := &WorkflowDraft{}
	next := counter()
	mustApply(t, d, next, toolAddStep, `{"type":"http_request"}`) // s1

	args := `{"stepId":"s1","controlValues":{"url":"https://example.com","method":"POST","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}}`
	if _, _, err := applyToolCall(d, reg(), next, toolEditStepContent, json.RawMessage(args)); err != nil {
		t.Fatalf("unexpected error for a valid http_request edit: %v", err)
	}
	var cv map[string]any
	_ = json.Unmarshal(d.Steps[0].ControlValues, &cv)
	if cv["method"] != "POST" {
		t.Errorf("method = %v, want POST", cv["method"])
	}
}

func TestUpdateStepConditions_SetsFilters(t *testing.T) {
	d := &WorkflowDraft{}
	next := counter()
	mustApply(t, d, next, toolAddStep, `{"type":"email"}`)
	mustApply(t, d, next, toolUpdateStepConditions,
		`{"stepId":"s1","filters":[{"combinator":"AND","conditions":[{"on":"payload","field":"vip","operator":"EQUAL","value":true}]}]}`)
	var groups []filter.Group
	if err := json.Unmarshal(d.Steps[0].Filters, &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Conditions) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	c := groups[0].Conditions[0]
	if c.On != "payload" || c.Field != "vip" || c.Operator != filter.OpEqual {
		t.Errorf("condition = %+v", c)
	}
}

func TestUpdateStepConditions_UnknownStepErrors(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolUpdateStepConditions,
		json.RawMessage(`{"stepId":"ghost","filters":[]}`)); err == nil {
		t.Fatal("expected error for unknown stepId")
	}
}

func TestRemoveStep_DeletesByID(t *testing.T) {
	d := &WorkflowDraft{}
	next := counter()
	mustApply(t, d, next, toolAddStep, `{"type":"email"}`) // s1
	mustApply(t, d, next, toolAddStep, `{"type":"sms"}`)   // s2
	mustApply(t, d, next, toolRemoveStep, `{"stepId":"s1"}`)
	if len(d.Steps) != 1 || d.Steps[0].StepID != "s2" {
		t.Fatalf("steps after remove = %+v", d.Steps)
	}
}

func TestRemoveStep_UnknownErrors(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolRemoveStep,
		json.RawMessage(`{"stepId":"ghost"}`)); err == nil {
		t.Fatal("expected error for unknown stepId")
	}
}

func TestMoveStep_Reorders(t *testing.T) {
	build := func() (*WorkflowDraft, idGen) {
		d := &WorkflowDraft{}
		next := counter()
		mustApply(t, d, next, toolAddStep, `{"type":"email"}`) // s1
		mustApply(t, d, next, toolAddStep, `{"type":"sms"}`)   // s2
		mustApply(t, d, next, toolAddStep, `{"type":"delay"}`) // s3
		return d, next
	}

	t.Run("to front", func(t *testing.T) {
		d, _ := build()
		mustApply(t, d, counter(), toolMoveStep, `{"stepId":"s3","newIndex":0}`)
		assertOrder(t, d, "s3", "s1", "s2")
	})
	t.Run("clamp high to end", func(t *testing.T) {
		d, _ := build()
		mustApply(t, d, counter(), toolMoveStep, `{"stepId":"s1","newIndex":99}`)
		assertOrder(t, d, "s2", "s3", "s1")
	})
	t.Run("clamp negative to front", func(t *testing.T) {
		d, _ := build()
		mustApply(t, d, counter(), toolMoveStep, `{"stepId":"s2","newIndex":-5}`)
		assertOrder(t, d, "s2", "s1", "s3")
	})
}

func TestMoveStep_UnknownErrors(t *testing.T) {
	d := &WorkflowDraft{}
	if _, _, err := applyToolCall(d, reg(), counter(), toolMoveStep,
		json.RawMessage(`{"stepId":"ghost","newIndex":0}`)); err == nil {
		t.Fatal("expected error for unknown stepId")
	}
}

func TestUpdatePayloadSchema_SetsSchema(t *testing.T) {
	d := &WorkflowDraft{}
	mustApply(t, d, counter(), toolUpdatePayloadSchema,
		`{"schema":{"type":"object","properties":{"name":{"type":"string"}}}}`)
	var m map[string]any
	if err := json.Unmarshal(d.PayloadSchema, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "object" {
		t.Errorf("schema = %v", m)
	}
}

func TestReasoning_EmitsTextNoDraftChange(t *testing.T) {
	d := &WorkflowDraft{Steps: []domain.Step{{StepID: "s1", Type: "email"}}}
	ev := mustApply(t, d, counter(), toolReasoning, `{"label":"Plan","thought":"I will add an email."}`)
	if ev.Kind != KindReasoning {
		t.Errorf("kind = %q, want reasoning", ev.Kind)
	}
	if ev.Text != "I will add an email." {
		t.Errorf("text = %q", ev.Text)
	}
	if len(d.Steps) != 1 {
		t.Errorf("reasoning mutated draft: %d steps", len(d.Steps))
	}
}

func assertOrder(t *testing.T, d *WorkflowDraft, ids ...string) {
	t.Helper()
	if len(d.Steps) != len(ids) {
		t.Fatalf("len = %d, want %d", len(d.Steps), len(ids))
	}
	for i, id := range ids {
		if d.Steps[i].StepID != id {
			got := make([]string, len(d.Steps))
			for j, s := range d.Steps {
				got[j] = s.StepID
			}
			t.Fatalf("order = %v, want %v", got, ids)
		}
	}
}
