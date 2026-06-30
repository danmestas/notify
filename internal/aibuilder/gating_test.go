package aibuilder

import (
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

func has(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

func TestAvailableStepTypes_ActionStepsAlwaysAvailable(t *testing.T) {
	// No integrations at all: only the action steps survive.
	got := AvailableStepTypes(steptype.Default(), nil)
	for _, action := range []string{"delay", "digest", "throttle", "http_request"} {
		if !has(got, action) {
			t.Errorf("action step %q should always be available, got %v", action, got)
		}
	}
	for _, channel := range []string{"in_app", "email", "push", "chat", "sms"} {
		if has(got, channel) {
			t.Errorf("channel %q should be gated out with no integrations, got %v", channel, got)
		}
	}
}

func TestAvailableStepTypes_OnlyEmailActive(t *testing.T) {
	ints := []domain.Integration{
		{Channel: "email", Active: true},
		{Channel: "sms", Active: false}, // present but inactive -> excluded
	}
	got := AvailableStepTypes(steptype.Default(), ints)
	if !has(got, "email") {
		t.Errorf("email should be available, got %v", got)
	}
	for _, excluded := range []string{"sms", "push", "in_app", "chat"} {
		if has(got, excluded) {
			t.Errorf("%q should be excluded (no active integration), got %v", excluded, got)
		}
	}
}

func TestAvailableStepTypes_FollowsRegistryOrder(t *testing.T) {
	ints := []domain.Integration{
		{Channel: "email", Active: true},
		{Channel: "in_app", Active: true},
	}
	got := AvailableStepTypes(steptype.Default(), ints)
	// Registry order is in_app, email, ... so in_app must precede email.
	iIn, iEmail := -1, -1
	for i, x := range got {
		switch x {
		case "in_app":
			iIn = i
		case "email":
			iEmail = i
		}
	}
	if iIn == -1 || iEmail == -1 || iIn > iEmail {
		t.Errorf("expected in_app before email in %v", got)
	}
}

func TestBuildTools_AddStepEnumIsGated(t *testing.T) {
	// Only email + the action steps are available.
	gated := AvailableStepTypes(steptype.Default(), []domain.Integration{{Channel: "email", Active: true}})
	tools := buildTools(gated)

	var addStep *ToolDef
	for i := range tools {
		if tools[i].Name == toolAddStep {
			addStep = &tools[i]
		}
	}
	if addStep == nil {
		t.Fatal("addStep tool not built")
	}

	enum := extractTypeEnum(t, addStep.InputSchema)
	if !has(enum, "email") {
		t.Errorf("addStep enum missing email: %v", enum)
	}
	for _, excluded := range []string{"sms", "push", "in_app", "chat"} {
		if has(enum, excluded) {
			t.Errorf("addStep enum should not contain unconfigured %q: %v", excluded, enum)
		}
	}
	// Action steps are always representable.
	for _, action := range []string{"delay", "digest", "throttle", "http_request"} {
		if !has(enum, action) {
			t.Errorf("addStep enum missing action step %q: %v", action, enum)
		}
	}
}

func TestBuildTools_HasNineTools(t *testing.T) {
	tools := buildTools([]string{"email", "delay"})
	if len(tools) != 9 {
		t.Fatalf("len(tools) = %d, want 9", len(tools))
	}
	want := map[string]bool{
		toolReasoning: true, toolSetWorkflowMetadata: true, toolAddStep: true,
		toolAddStepInBetween: true, toolEditStepContent: true, toolUpdateStepConditions: true,
		toolRemoveStep: true, toolMoveStep: true, toolUpdatePayloadSchema: true,
	}
	for _, td := range tools {
		if !want[td.Name] {
			t.Errorf("unexpected tool %q", td.Name)
		}
		delete(want, td.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

// extractTypeEnum pulls the `type` property enum out of an addStep input schema.
func extractTypeEnum(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var s struct {
		Properties struct {
			Type struct {
				Enum []string `json:"enum"`
			} `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.Properties.Type.Enum
}
