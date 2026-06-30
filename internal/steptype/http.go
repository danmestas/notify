package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// httpRequest is the http_request ACTION step type. It compiles to a normal
// "http_request" engine task that makes an outbound HTTP call mid-workflow with
// variable-interpolated url/headers/body, captures the response
// (statusCode/body/headers) into step_outputs, and exposes it to downstream
// steps. The handler reads url/method/headers/body/timeoutSec/failurePolicy
// from the step's controlValues at run time (no Config travels in the StepDef).
//
// headers is stored as a JSON-encoded STRING value (e.g. `{"Authorization":
// "Bearer x"}`) rather than a nested object so it round-trips through the flat
// controlMap contract every other step type uses; the handler does a second
// json.Unmarshal on the string.
type httpRequest struct{}

func (httpRequest) Name() string { return "http_request" }

func (httpRequest) DefaultControlValues() json.RawMessage {
	return json.RawMessage(`{"url":"","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`)
}

func (httpRequest) ControlMap(cv json.RawMessage) map[string]string {
	return flattenControlValues(cv)
}

func (httpRequest) EditorPartial() string { return "wf-step-fields-http_request" }

func (httpRequest) ParseForm(form url.Values) (json.RawMessage, error) {
	timeoutSec, err := strconv.Atoi(formField(form, "timeoutSec"))
	if err != nil || timeoutSec < 1 {
		// An absent/blank timeout falls back to the 30s default rather than
		// failing the save; only a present-but-invalid value is rejected.
		if formField(form, "timeoutSec") == "" {
			timeoutSec = 30
		} else {
			return nil, fmt.Errorf("timeoutSec must be a positive whole number")
		}
	}
	method := formField(form, "method")
	if method == "" {
		method = "GET"
	}
	failurePolicy := formField(form, "failurePolicy")
	if failurePolicy == "" {
		failurePolicy = "fail_closed"
	}
	return marshalControlValues(map[string]any{
		"url":           formField(form, "url"),
		"method":        method,
		"headers":       formField(form, "headers"),
		"body":          formField(form, "body"),
		"timeoutSec":    timeoutSec,
		"failurePolicy": failurePolicy,
	})
}

func (httpRequest) Validate(cv json.RawMessage) error {
	_, err := httpRequestConfig(cv)
	return err
}

func (httpRequest) Compile(step domain.Step) (engine.StepDef, error) {
	if _, err := httpRequestConfig(step.ControlValues); err != nil {
		return engine.StepDef{}, err
	}
	// No Config: the handler loads url/method/headers/body/timeout/policy from
	// the store at run time, mirroring every other normal-task step type.
	return engine.StepDef{ID: step.StepID, Task: "http_request", Type: "normal"}, nil
}

// httpRequestCfg holds the validated control values for an http_request step.
type httpRequestCfg struct {
	URL           string `json:"url"`
	Method        string `json:"method"`
	Headers       string `json:"headers"`
	Body          string `json:"body"`
	TimeoutSec    int    `json:"timeoutSec"`
	FailurePolicy string `json:"failurePolicy"`
}

// httpRequestConfig parses an http_request step's controlValues and validates
// the closed-set fields (method, failurePolicy). url is permitted to be empty
// at authoring time (a freshly added step) — it is interpolated and dialed only
// at run time. timeoutSec defaults to 30 when absent/zero.
func httpRequestConfig(cv json.RawMessage) (httpRequestCfg, error) {
	var cfg httpRequestCfg
	if len(cv) > 0 {
		if err := json.Unmarshal(cv, &cfg); err != nil {
			return httpRequestCfg{}, fmt.Errorf("http_request: invalid control values: %w", err)
		}
	}
	if cfg.Method == "" {
		cfg.Method = "GET"
	}
	switch cfg.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return httpRequestCfg{}, fmt.Errorf("http_request: method %q is not one of GET/POST/PUT/PATCH/DELETE", cfg.Method)
	}
	if cfg.FailurePolicy == "" {
		cfg.FailurePolicy = "fail_closed"
	}
	switch cfg.FailurePolicy {
	case "fail_closed", "continue":
	default:
		return httpRequestCfg{}, fmt.Errorf("http_request: failurePolicy %q is not one of fail_closed/continue", cfg.FailurePolicy)
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 30
	}
	return cfg, nil
}
