package steptype_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestAgentMessage_Compile_IsNormalTask: an agent_message step compiles to a
// normal "agent_message" task with no Config, including the default seed of a
// freshly added step.
func TestAgentMessage_Compile_IsNormalTask(t *testing.T) {
	st := steptype.Default().Get("agent_message")
	for _, cv := range []json.RawMessage{
		json.RawMessage(`{"agent":"support","text":"hi {{payload.name}}","conversation":"","failurePolicy":"continue"}`),
		st.DefaultControlValues(),
	} {
		sd, err := st.Compile(domain.Step{StepID: "a1", Type: "agent_message", ControlValues: cv})
		if err != nil {
			t.Fatalf("Compile(%s): %v", cv, err)
		}
		if sd.ID != "a1" || sd.Task != "notify.agent_message" || sd.Type != dag.StepTypeNormal || sd.Config != nil {
			t.Errorf("Compile(%s) = %+v", cv, sd)
		}
	}
}

func TestAgentMessage_Validate_FailurePolicy(t *testing.T) {
	st := steptype.Default().Get("agent_message")
	if err := st.Validate(json.RawMessage(`{"agent":"a","text":"t","failurePolicy":"retry"}`)); err == nil {
		t.Error("failurePolicy retry accepted, want an error")
	}
	if err := st.Validate(json.RawMessage(`{"agent":"a","text":"t"}`)); err != nil {
		t.Errorf("absent failurePolicy: %v", err)
	}
}

func TestAgentMessage_ParseForm(t *testing.T) {
	st := steptype.Default().Get("agent_message")
	cv, err := st.ParseForm(url.Values{
		"agent":        {" support "},
		"text":         {"New mail: {{payload.subject}}"},
		"conversation": {"{{payload.thread}}"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	got := st.ControlMap(cv)
	want := map[string]string{
		"agent": "support", "text": "New mail: {{payload.subject}}",
		"conversation": "{{payload.thread}}", "failurePolicy": "fail_closed",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
