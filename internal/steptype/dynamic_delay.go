package steptype

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// DelayInputPath is the run-input object the native sleep of a dynamic delay
// reads its deadline from: delays.<stepID>. The trigger resolves each dynamic
// delay's payload value against the step's declared kind and writes it there,
// so the engine never interprets a payload value the step did not declare.
const DelayInputPath = "delays"

// DynamicDelayInput resolves a dynamic delay's payload value into what its
// native sleep reads at delays.<stepID>: an RFC3339 instant for
// dynamicKind=instant, or whole milliseconds for dynamicKind=relative, which
// the engine counts from when the sleep step starts.
//
// It runs at trigger time, once per run. A mismatch between the declared kind
// and the value is an error rather than a reinterpretation (#258).
func DynamicDelayInput(payload json.RawMessage, dynamicKey, dynamicKind string) (json.RawMessage, error) {
	node, err := PayloadValue(payload, dynamicKey)
	if err != nil {
		return nil, err
	}
	switch dynamicKind {
	case DynamicKindInstant:
		var s string
		if uErr := json.Unmarshal(node, &s); uErr != nil || s == "" {
			return nil, fmt.Errorf("value at %q must be an RFC3339 string (step declares dynamicKind=%q)", dynamicKey, DynamicKindInstant)
		}
		if _, pErr := time.Parse(time.RFC3339, s); pErr != nil {
			return nil, fmt.Errorf("value at %q (%q) is not an RFC3339 timestamp: %w", dynamicKey, s, pErr)
		}
		return json.Marshal(s)
	case DynamicKindRelative:
		d, rErr := RelativeDuration(node, dynamicKey)
		if rErr != nil {
			return nil, rErr
		}
		return json.Marshal(d.Milliseconds())
	default:
		return nil, fmt.Errorf("unknown dynamicKind %q", dynamicKind)
	}
}

// RelativeDuration reads a relative dynamic delay's {amount,unit} value. The
// unit vocabulary is UnitDuration's, so "months" is 30 days here as elsewhere.
func RelativeDuration(node json.RawMessage, dynamicKey string) (time.Duration, error) {
	var obj struct {
		Amount int64  `json:"amount"`
		Unit   string `json:"unit"`
	}
	if err := json.Unmarshal(node, &obj); err != nil {
		return 0, fmt.Errorf("value at %q must be an {amount,unit} object (step declares dynamicKind=%q)", dynamicKey, DynamicKindRelative)
	}
	if obj.Amount <= 0 {
		return 0, fmt.Errorf("value at %q has non-positive amount %d", dynamicKey, obj.Amount)
	}
	unit, err := UnitDuration(obj.Unit)
	if err != nil {
		return 0, err
	}
	return time.Duration(obj.Amount) * unit, nil
}

// PayloadValue walks a "payload.a.b" dot-path against the trigger payload and
// returns the raw JSON value at the leaf. The first segment must be "payload";
// a missing or non-object intermediate or leaf is an error.
func PayloadValue(payload json.RawMessage, dynamicKey string) (json.RawMessage, error) {
	parts := strings.Split(dynamicKey, ".")
	if len(parts) < 2 || parts[0] != "payload" {
		return nil, fmt.Errorf("dynamicKey %q must be a payload dot-path (e.g. payload.sendAt)", dynamicKey)
	}
	cur := payload
	for _, p := range parts[1:] {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(cur, &m); err != nil {
			return nil, fmt.Errorf("dynamicKey %q: %q is not inside an object", dynamicKey, p)
		}
		next, ok := m[p]
		if !ok {
			return nil, fmt.Errorf("dynamicKey %q: missing %q in payload", dynamicKey, p)
		}
		cur = next
	}
	return cur, nil
}

// nativeDynamicDelay reports whether a dynamic delay compiles to the engine's
// native sleep. A step id with a '.' cannot be a dot-path segment and a step
// with no declared kind cannot be resolved at trigger time; both keep the
// delay.dynamic task, which fails them at dispatch as it always has.
func nativeDynamicDelay(stepID string, cv delayControlValues) bool {
	return stepID != "" && !strings.Contains(stepID, ".") && validDynamicKind(cv.DynamicKind)
}

// NativeDynamicDelay reports whether step is a dynamic delay that compiles to
// the engine's native sleep, and if so its dynamicKey and dynamicKind, so the
// trigger resolves exactly the steps Compile pointed at delays.<stepID>.
func NativeDynamicDelay(step domain.Step) (dynamicKey, dynamicKind string, ok bool) {
	if step.Type != "delay" {
		return "", "", false
	}
	cv, err := delayType(step.ControlValues)
	if err != nil || cv.Type != "dynamic" || cv.DynamicKey == "" || !nativeDynamicDelay(step.StepID, cv) {
		return "", "", false
	}
	return cv.DynamicKey, cv.DynamicKind, true
}
