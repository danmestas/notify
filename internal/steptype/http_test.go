package steptype_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestHTTPRequest_Compile_IsNormalTask is the guard ensuring the engine handler
// task name matches what the registry emits: an http_request step must compile
// to a normal "http_request" task (NOT a "send.http_request" channel task and
// NOT a "sleep"), carrying no Config (the handler loads controlValues at run
// time).
func TestHTTPRequest_Compile_IsNormalTask(t *testing.T) {
	r := steptype.Default()
	sd, err := r.Get("http_request").Compile(domain.Step{
		StepID: "h1", Type: "http_request",
		ControlValues: json.RawMessage(`{"url":"https://example.com","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`),
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if sd.Task != "http_request" {
		t.Errorf("Task = %q, want http_request", sd.Task)
	}
	if sd.Type != "normal" {
		t.Errorf("Type = %q, want normal", sd.Type)
	}
	if sd.ID != "h1" {
		t.Errorf("ID = %q, want h1", sd.ID)
	}
	if sd.Config != nil {
		t.Errorf("Config = %s, want nil (handler loads config from store)", sd.Config)
	}
}

// TestHTTPRequest_Compile_EmptyControlValues proves a freshly added step (the
// default seed, empty url) still compiles: url is dialed only at run time, so
// authoring must not reject an empty url.
func TestHTTPRequest_Compile_EmptyControlValues(t *testing.T) {
	r := steptype.Default()
	if _, err := r.Get("http_request").Compile(domain.Step{
		StepID: "h", Type: "http_request",
		ControlValues: r.Get("http_request").DefaultControlValues(),
	}); err != nil {
		t.Fatalf("Compile of default seed: %v", err)
	}
}

// TestHTTPRequest_Compile_BadMethod rejects an unknown HTTP method at compile
// time so the workflow is refused with HTTP 422 rather than failing at run time.
func TestHTTPRequest_Compile_BadMethod(t *testing.T) {
	r := steptype.Default()
	if _, err := r.Get("http_request").Compile(domain.Step{
		StepID: "h", Type: "http_request",
		ControlValues: json.RawMessage(`{"url":"https://x","method":"FETCH","failurePolicy":"fail_closed"}`),
	}); err == nil {
		t.Fatal("Compile: expected error for method=FETCH, got nil")
	}
}

// TestHTTPRequest_Compile_BadFailurePolicy rejects an unknown failure policy.
func TestHTTPRequest_Compile_BadFailurePolicy(t *testing.T) {
	r := steptype.Default()
	if _, err := r.Get("http_request").Compile(domain.Step{
		StepID: "h", Type: "http_request",
		ControlValues: json.RawMessage(`{"url":"https://x","method":"GET","failurePolicy":"explode"}`),
	}); err == nil {
		t.Fatal("Compile: expected error for failurePolicy=explode, got nil")
	}
}

// TestHTTPRequest_DefaultControlValues verifies the seeded config is valid JSON
// carrying the flat fields the editor form and handler expect.
func TestHTTPRequest_DefaultControlValues(t *testing.T) {
	r := steptype.Default()
	raw := r.Get("http_request").DefaultControlValues()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("DefaultControlValues is not valid JSON: %v", err)
	}
	for _, k := range []string{"url", "method", "headers", "body", "timeoutSec", "failurePolicy"} {
		if _, ok := m[k]; !ok {
			t.Errorf("default control values missing %q: %s", k, raw)
		}
	}
	if m["method"] != "GET" {
		t.Errorf("default method = %v, want GET", m["method"])
	}
	if m["failurePolicy"] != "fail_closed" {
		t.Errorf("default failurePolicy = %v, want fail_closed", m["failurePolicy"])
	}
}

// TestHTTPRequest_ParseForm_RoundTrips proves the editor form fields flatten
// into the controlValues shape the handler reads back.
func TestHTTPRequest_ParseForm_RoundTrips(t *testing.T) {
	r := steptype.Default()
	form := url.Values{
		"url":           {"https://api.example.com/{{payload.id}}"},
		"method":        {"POST"},
		"headers":       {`{"Authorization":"Bearer {{payload.token}}"}`},
		"body":          {`{"hi":"{{subscriber.firstName}}"}`},
		"timeoutSec":    {"15"},
		"failurePolicy": {"continue"},
	}
	got, err := r.Get("http_request").ParseForm(form)
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if m["url"] != "https://api.example.com/{{payload.id}}" {
		t.Errorf("url = %v", m["url"])
	}
	if m["method"] != "POST" {
		t.Errorf("method = %v, want POST", m["method"])
	}
	if v, _ := m["timeoutSec"].(float64); v != 15 {
		t.Errorf("timeoutSec = %v, want 15", v)
	}
	if m["failurePolicy"] != "continue" {
		t.Errorf("failurePolicy = %v, want continue", m["failurePolicy"])
	}
}
