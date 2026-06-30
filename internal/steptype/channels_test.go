package steptype

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
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
