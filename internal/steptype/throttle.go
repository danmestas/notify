package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/danmestas/dagnats/dag"

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
	cv := map[string]any{"limit": limit, "windowSec": windowSec}
	// Omitted when blank so an unset key never appears in stored control
	// values -- absent means "one budget per (env, subscriber, workflow)",
	// which is the pre-throttleKey behaviour.
	if k := formField(form, "throttleKey"); k != "" {
		cv["throttleKey"] = k
	}
	return marshalControlValues(cv)
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
	return engine.StepDef{ID: step.StepID, Task: "check.throttle", Type: dag.StepTypeNormal}, nil
}

// throttleCfg holds the validated control values for a throttle step.
type throttleCfg struct {
	Limit     int `json:"limit"`
	WindowSec int `json:"windowSec"`
	// ThrottleKey is an optional payload dot-path (e.g. "payload.check").
	// Its resolved value is appended to the throttle key so unrelated alerts
	// on one workflow get their own budget instead of competing (#295).
	ThrottleKey string `json:"throttleKey,omitempty"`
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
	if k := cfg.ThrottleKey; k != "" {
		if !strings.HasPrefix(k, "payload.") || len(k) <= len("payload.") {
			// subscriber-rooted paths are rejected rather than ignored: the
			// base key already contains the subscriber id, so partitioning by
			// it again would be a no-op the author could not see.
			return throttleCfg{}, fmt.Errorf(
				"throttle throttleKey must be a payload dot-path like \"payload.check\", got %q "+
					"(the budget is already per-subscriber, so subscriber paths add nothing)", k,
			)
		}
	}
	return cfg, nil
}
