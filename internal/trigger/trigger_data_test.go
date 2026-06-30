package trigger

import (
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/runenv"
)

// TestStepControls_DataJSON_Unmarshal verifies that runenv.StepControls
// correctly deserializes data_json as a raw JSON string from the ControlValues
// blob, and that the resulting string can be round-tripped to map[string]string.
func TestStepControls_DataJSON_Unmarshal(t *testing.T) {
	raw := json.RawMessage(`{"title":"Alert","body":"Body","data_json":"{\"screen\":\"home\",\"badge\":\"3\"}"}`)
	var controls runenv.StepControls
	if err := json.Unmarshal(raw, &controls); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	const want = `{"screen":"home","badge":"3"}`
	if controls.DataJSON != want {
		t.Errorf("DataJSON = %q, want %q", controls.DataJSON, want)
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(controls.DataJSON), &data); err != nil {
		t.Fatalf("Unmarshal DataJSON to map: %v", err)
	}
	if data["screen"] != "home" {
		t.Errorf("data[screen] = %q, want %q", data["screen"], "home")
	}
	if data["badge"] != "3" {
		t.Errorf("data[badge] = %q, want %q", data["badge"], "3")
	}
}

// TestStepControls_DataJSON_EmptyOmitted verifies that absent data_json leaves
// DataJSON as empty string (zero value).
func TestStepControls_DataJSON_EmptyOmitted(t *testing.T) {
	raw := json.RawMessage(`{"title":"Alert","body":"Body"}`)
	var controls runenv.StepControls
	if err := json.Unmarshal(raw, &controls); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if controls.DataJSON != "" {
		t.Errorf("DataJSON = %q, want empty string", controls.DataJSON)
	}
}
