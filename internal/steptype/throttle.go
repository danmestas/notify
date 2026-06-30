package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// throttle is the standalone throttle step type. It compiles to a normal
// "check.throttle" engine task that gates an entire workflow run by a
// fixed-window counter keyed per (envID, subscriberID, workflowID). Under the
// limit the run proceeds; at or over the limit the run fails permanently — no
// downstream channel steps execute and no message row is written.
type throttle struct{}

func (throttle) Name() string        { return "throttle" }
func (throttle) DisplayName() string { return "Throttle" }

func (throttle) DefaultControlValues() json.RawMessage {
	return json.RawMessage(`{"limit":10,"windowSec":3600}`)
}

func (throttle) ControlMap(cv json.RawMessage) map[string]string {
	return flattenControlValues(cv)
}

func (throttle) EditorPartial() string { return "wf-step-fields-throttle" }

func (throttle) ParseForm(form url.Values) (json.RawMessage, error) {
	limit, err := strconv.Atoi(formField(form, "limit"))
	if err != nil || limit < 1 {
		return nil, fmt.Errorf("limit must be a positive whole number")
	}
	windowSec, err := strconv.Atoi(formField(form, "windowSec"))
	if err != nil || windowSec < 1 {
		return nil, fmt.Errorf("windowSec must be a positive whole number")
	}
	return marshalControlValues(map[string]any{"limit": limit, "windowSec": windowSec})
}

func (throttle) Validate(cv json.RawMessage) error {
	_, err := throttleConfig(cv)
	return err
}

func (throttle) Compile(step domain.Step) (engine.StepDef, error) {
	if _, err := throttleConfig(step.ControlValues); err != nil {
		return engine.StepDef{}, err
	}
	// No Config: the handler loads limit/windowSec from the store at run time
	// using the workflow row, following the same pattern as other task handlers.
	return engine.StepDef{ID: step.StepID, Task: "check.throttle", Type: "normal"}, nil
}

// throttleCfg holds the validated control values for a throttle step.
type throttleCfg struct {
	Limit     int `json:"limit"`
	WindowSec int `json:"windowSec"`
}

// throttleConfig parses and validates a throttle step's controlValues. Both
// Limit and WindowSec must be strictly positive integers.
func throttleConfig(cv json.RawMessage) (throttleCfg, error) {
	var cfg throttleCfg
	if len(cv) > 0 {
		if err := json.Unmarshal(cv, &cfg); err != nil {
			return throttleCfg{}, fmt.Errorf("throttle: invalid control values: %w", err)
		}
	}
	if cfg.Limit <= 0 {
		return throttleCfg{}, fmt.Errorf("throttle limit must be positive, got %d", cfg.Limit)
	}
	if cfg.WindowSec <= 0 {
		return throttleCfg{}, fmt.Errorf("throttle windowSec must be positive, got %d", cfg.WindowSec)
	}
	return cfg, nil
}
