package steptype_test

import (
	"encoding/json"
	"testing"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestCompile_DynamicDelayIsNativeSleep: a dynamic delay with a declared kind
// compiles to the engine's sleep, reading its deadline from delays.<stepID>,
// so no notify worker is held for the wait.
func TestCompile_DynamicDelayIsNativeSleep(t *testing.T) {
	for _, kind := range []string{steptype.DynamicKindInstant, steptype.DynamicKindRelative} {
		def, err := steptype.Default().Get("delay").Compile(domain.Step{
			StepID: "d1", Type: "delay",
			ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"` + kind + `"}`),
		})
		if err != nil {
			t.Fatalf("%s: compile: %v", kind, err)
		}
		if def.Type != dag.StepTypeSleep || def.Task != "" || def.Retry != nil {
			t.Fatalf("%s: def = %+v, want a sleep with no task or retry", kind, def)
		}
		if string(def.Config) != `{"until_input_path":"delays.d1"}` {
			t.Fatalf("%s: config = %s", kind, def.Config)
		}
	}
}

// TestCompile_DynamicDelayDottedStepIDKeepsTask: a step id with a '.' cannot
// be a dot-path segment, so that step keeps the delay.dynamic task, and the
// trigger does not resolve it either.
func TestCompile_DynamicDelayDottedStepIDKeepsTask(t *testing.T) {
	step := domain.Step{
		StepID: "wait.1", Type: "delay",
		ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"relative"}`),
	}
	def, err := steptype.Default().Get("delay").Compile(step)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.Type != dag.StepTypeNormal || def.Task != "notify.delay.dynamic" {
		t.Fatalf("def = %+v, want the delay.dynamic task", def)
	}
	if _, _, ok := steptype.NativeDynamicDelay(step); ok {
		t.Fatal("NativeDynamicDelay reports a step Compile did not make native")
	}
}

func TestDynamicDelayInput(t *testing.T) {
	cases := []struct {
		name, payload, kind, want string
	}{
		{"instant", `{"wait":"2027-01-02T03:04:05Z"}`, steptype.DynamicKindInstant, `"2027-01-02T03:04:05Z"`},
		{"instant in the past is kept", `{"wait":"2001-01-01T00:00:00Z"}`, steptype.DynamicKindInstant, `"2001-01-01T00:00:00Z"`},
		{"relative minutes", `{"wait":{"amount":90,"unit":"minutes"}}`, steptype.DynamicKindRelative, `5400000`},
		{"relative months is 30 days", `{"wait":{"amount":1,"unit":"months"}}`, steptype.DynamicKindRelative, `2592000000`},
	}
	for _, c := range cases {
		got, err := steptype.DynamicDelayInput(json.RawMessage(c.payload), "payload.wait", c.kind)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Fatalf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// TestDynamicDelayInput_KindMismatchIsAnError: the engine reads a bare number
// as milliseconds, so a number sent to an instant step must be refused here
// rather than silently reinterpreted (#258).
func TestDynamicDelayInput_KindMismatchIsAnError(t *testing.T) {
	cases := []struct{ name, payload, kind string }{
		{"number for instant", `{"wait":5000}`, steptype.DynamicKindInstant},
		{"bad instant", `{"wait":"tomorrow"}`, steptype.DynamicKindInstant},
		{"string for relative", `{"wait":"2027-01-02T03:04:05Z"}`, steptype.DynamicKindRelative},
		{"zero amount", `{"wait":{"amount":0,"unit":"hours"}}`, steptype.DynamicKindRelative},
		{"unknown unit", `{"wait":{"amount":1,"unit":"fortnights"}}`, steptype.DynamicKindRelative},
		{"missing key", `{"other":1}`, steptype.DynamicKindRelative},
	}
	for _, c := range cases {
		if got, err := steptype.DynamicDelayInput(json.RawMessage(c.payload), "payload.wait", c.kind); err == nil {
			t.Fatalf("%s: got %s, want an error", c.name, got)
		}
	}
}

// TestPayloadValue_Nested proves the dot-path walks nested objects and returns
// the raw leaf value.
func TestPayloadValue_Nested(t *testing.T) {
	node, err := steptype.PayloadValue(json.RawMessage(`{"a":{"b":{"c":"deep"}}}`), "payload.a.b.c")
	if err != nil {
		t.Fatalf("PayloadValue: %v", err)
	}
	if string(node) != `"deep"` {
		t.Fatalf("node = %s, want \"deep\"", node)
	}
}
