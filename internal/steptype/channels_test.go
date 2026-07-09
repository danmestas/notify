package steptype

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

const wantDataErrMsg = "data must be a JSON object with string values only"

// -- ParseForm tests --

func TestPushParseForm_NullValue_Rejected(t *testing.T) {
	form := url.Values{
		"title":     {"Alert"},
		"body":      {"Body"},
		"data_json": {`{"key": null}`},
	}
	_, err := (push{}).ParseForm(form)
	if err == nil {
		t.Fatal("ParseForm: expected error for null value, got nil")
	}
	if !strings.Contains(err.Error(), wantDataErrMsg) {
		t.Errorf("ParseForm error = %q, want it to contain %q", err.Error(), wantDataErrMsg)
	}
}

func TestPushParseForm_NonStringValue_Rejected(t *testing.T) {
	form := url.Values{
		"title":     {"Alert"},
		"body":      {"Body"},
		"data_json": {`{"count": 1}`},
	}
	_, err := (push{}).ParseForm(form)
	if err == nil {
		t.Fatal("ParseForm: expected error for numeric value, got nil")
	}
	if !strings.Contains(err.Error(), wantDataErrMsg) {
		t.Errorf("ParseForm error = %q, want it to contain %q", err.Error(), wantDataErrMsg)
	}
}

func TestPushParseForm_ValidStringMap_Accepted(t *testing.T) {
	form := url.Values{
		"title":     {"Alert"},
		"body":      {"Body"},
		"data_json": {`{"screen":"home","badge":"3"}`},
	}
	raw, err := (push{}).ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: unexpected error: %v", err)
	}
	var cv struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		DataJSON string `json:"data_json"`
	}
	if err := json.Unmarshal(raw, &cv); err != nil {
		t.Fatalf("Unmarshal controlValues: %v", err)
	}
	if cv.DataJSON != `{"screen":"home","badge":"3"}` {
		t.Errorf("DataJSON = %q, want %q", cv.DataJSON, `{"screen":"home","badge":"3"}`)
	}
}

func TestPushParseForm_EmptyDataJSON_Accepted(t *testing.T) {
	form := url.Values{"title": {"Alert"}, "body": {"Body"}}
	raw, err := (push{}).ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: unexpected error with no data_json: %v", err)
	}
	var cv struct {
		DataJSON string `json:"data_json"`
	}
	if err := json.Unmarshal(raw, &cv); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cv.DataJSON != "" {
		t.Errorf("DataJSON = %q, want empty when absent", cv.DataJSON)
	}
}

// -- Validate tests --

func TestPushValidate_NullValue_Rejected(t *testing.T) {
	// data_json string inside controlValues contains a null value.
	cv := json.RawMessage(`{"title":"Alert","body":"Body","data_json":"{\"key\":null}"}`)
	err := (push{}).Validate(cv)
	if err == nil {
		t.Fatal("Validate: expected error for null value, got nil")
	}
	if !strings.Contains(err.Error(), wantDataErrMsg) {
		t.Errorf("Validate error = %q, want it to contain %q", err.Error(), wantDataErrMsg)
	}
}

func TestPushValidate_NonStringValue_Rejected(t *testing.T) {
	cv := json.RawMessage(`{"title":"Alert","body":"Body","data_json":"{\"k\":1}"}`)
	err := (push{}).Validate(cv)
	if err == nil {
		t.Fatal("Validate: expected error for numeric value, got nil")
	}
	if !strings.Contains(err.Error(), wantDataErrMsg) {
		t.Errorf("Validate error = %q, want it to contain %q", err.Error(), wantDataErrMsg)
	}
}

func TestPushValidate_ValidStringMap_Accepted(t *testing.T) {
	cv := json.RawMessage(`{"title":"Alert","body":"Body","data_json":"{\"screen\":\"home\"}"}`)
	if err := (push{}).Validate(cv); err != nil {
		t.Fatalf("Validate: unexpected error for valid string map: %v", err)
	}
}

func TestPushValidate_NoDataJSON_Accepted(t *testing.T) {
	cv := json.RawMessage(`{"title":"Alert","body":"Body"}`)
	if err := (push{}).Validate(cv); err != nil {
		t.Fatalf("Validate: unexpected error when data_json absent: %v", err)
	}
}

// -- stream tests --

// TestStream_ParseForm proves the controlValues surface for a stream step is
// exactly {topic, body} — trimmed, no extra keys.
func TestStream_ParseForm(t *testing.T) {
	form := url.Values{
		"topic": {" orders.created "},
		"body":  {"hi {{payload.x}}"},
	}
	got, err := (stream{}).ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if string(got) != `{"body":"hi {{payload.x}}","topic":"orders.created"}` {
		t.Fatalf("ParseForm = %s", got)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("ParseForm produced %d keys, want exactly 2: %v", len(m), m)
	}
}

// TestStream_Validate exercises the authoring-time static topic guard: empty
// topics and topics containing wildcard/injection characters are rejected;
// dot-separated and templated topics are accepted (the rendered value is
// re-validated at send time by domain.ValidateFeedTopic, not here).
func TestStream_Validate(t *testing.T) {
	cases := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"plain dotted topic", "orders.created", false},
		{"templated topic", "orders.{{payload.region}}.created", false},
		{"templated topic with spaces in braces", "orders.{{ payload.region }}.created", false},
		{"templated topic no spaces around field", "orders.{{payload.x}}", false},
		{"empty topic", "", true},
		{"wildcard star", "orders.*", true},
		{"wildcard gt", "a.>", true},
		{"whitespace", "a b", true},
		{"dollar sign", "$SYS.x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cv, err := marshalControlValues(map[string]any{"topic": tc.topic, "body": "b"})
			if err != nil {
				t.Fatalf("marshalControlValues: %v", err)
			}
			err = (stream{}).Validate(cv)
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.topic)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.topic, err)
			}
		})
	}
	if err := (stream{}).Validate(json.RawMessage(`{"topic":""}`)); err == nil || err.Error() != "stream: topic is required" {
		t.Errorf("Validate(empty topic) error = %v, want %q", err, "stream: topic is required")
	}
}

// TestStream_Compile proves a stream step compiles to the send.stream normal
// task (the channelStepDef shared helper), matching the sibling channel types.
func TestStream_Compile(t *testing.T) {
	sd, err := (stream{}).Compile(domain.Step{StepID: "S1", Type: "stream", ControlValues: json.RawMessage(`{"topic":"orders.created","body":"hi"}`)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := engine.StepDef{ID: "S1", Task: "send.stream", Type: "normal"}
	if !reflect.DeepEqual(sd, want) {
		t.Fatalf("Compile = %+v, want %+v", sd, want)
	}
}
