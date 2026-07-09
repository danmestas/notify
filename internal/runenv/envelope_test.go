package runenv_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/runenv"
)

// TestStepControls_TopicRoundTrip proves the stream channel's Topic field
// survives a marshal->unmarshal round trip through StepControls.
func TestStepControls_TopicRoundTrip(t *testing.T) {
	in := runenv.StepControls{Topic: "orders.{{payload.orderId}}"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out runenv.StepControls
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Topic != "orders.{{payload.orderId}}" {
		t.Errorf("Topic round trip = %q, want %q", out.Topic, "orders.{{payload.orderId}}")
	}
}

// TestStepControls_TopicAbsent_NoKeyInJSON is the golden-compat guard: an
// absent Topic must NOT add a "topic" key to the marshaled JSON, so every
// non-stream channel's envelope stays byte-identical to before this field
// existed.
func TestStepControls_TopicAbsent_NoKeyInJSON(t *testing.T) {
	in := runenv.StepControls{Subject: "S", Body: "B"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"topic"`) {
		t.Errorf("marshaled StepControls contains a topic key with Topic unset: %s", raw)
	}
}

// TestStep_TopicAbsent_NoKeyInJSON mirrors the guard above for runenv.Step,
// the field the trigger and rebuild paths actually populate onto the wire
// envelope.
func TestStep_TopicAbsent_NoKeyInJSON(t *testing.T) {
	in := runenv.Step{StepID: "S1", Channel: "in_app", BodyTemplate: "hi"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"topic"`) {
		t.Errorf("marshaled Step contains a topic key with Topic unset: %s", raw)
	}
}

// TestStep_TopicRoundTrip proves Step.Topic survives marshal->unmarshal too.
func TestStep_TopicRoundTrip(t *testing.T) {
	in := runenv.Step{StepID: "S1", Channel: "stream", Topic: "orders.{{payload.r}}"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out runenv.Step
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Topic != "orders.{{payload.r}}" {
		t.Errorf("Topic round trip = %q, want %q", out.Topic, "orders.{{payload.r}}")
	}
}
