package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// sleepControlValues is the default window for a freshly added delay/digest
// step: a positive amount is required or Compile fails, so seed 1 minute.
func sleepControlValues() json.RawMessage {
	return json.RawMessage(`{"amount":1,"unit":"minutes"}`)
}

// sleepConfig parses a delay/digest step's controlValues
// ({"amount":<int>,"unit":...}) into a dag.SleepConfig JSON
// ({"duration":<int64 ns>}). Unit defaults to seconds; amount must be positive.
// The error strings are preserved verbatim from the pre-registry authoring
// helper so the HTTP 422 surface is byte-identical.
func sleepConfig(controlValues json.RawMessage) (json.RawMessage, error) {
	var cv struct {
		Amount int64  `json:"amount"`
		Unit   string `json:"unit"`
	}
	if len(controlValues) > 0 {
		if err := json.Unmarshal(controlValues, &cv); err != nil {
			return nil, fmt.Errorf("unmarshal delay controlValues: %w", err)
		}
	}
	if cv.Amount <= 0 {
		return nil, fmt.Errorf("delay amount must be positive, got %d", cv.Amount)
	}
	var unit time.Duration
	switch cv.Unit {
	case "", "seconds":
		unit = time.Second
	case "minutes":
		unit = time.Minute
	case "hours":
		unit = time.Hour
	case "days":
		unit = 24 * time.Hour
	default:
		return nil, fmt.Errorf("unknown delay unit %q", cv.Unit)
	}
	cfg, err := json.Marshal(struct {
		Duration time.Duration `json:"duration"`
	}{Duration: time.Duration(cv.Amount) * unit})
	if err != nil {
		return nil, fmt.Errorf("marshal sleep config: %w", err)
	}
	return cfg, nil
}

// sleepStepDef builds the engine "sleep" StepDef from a step's controlValues.
func sleepStepDef(step domain.Step) (engine.StepDef, error) {
	cfg, err := sleepConfig(step.ControlValues)
	if err != nil {
		return engine.StepDef{}, err
	}
	return engine.StepDef{ID: step.StepID, Type: "sleep", Config: cfg}, nil
}

// parseSleepForm builds a delay/digest step's controlValues from the editor's
// amount + unit fields. Preserves the historical validation message and the
// "minutes" unit default.
func parseSleepForm(form url.Values) (json.RawMessage, error) {
	amount, err := strconv.Atoi(formField(form, "amount"))
	if err != nil || amount < 1 {
		return nil, fmt.Errorf("amount must be a positive whole number")
	}
	unit := formField(form, "unit")
	if unit == "" {
		unit = "minutes"
	}
	return marshalControlValues(map[string]any{"amount": amount, "unit": unit})
}

// delay is the delay step type: it compiles to an engine sleep the engine times
// itself. It has its OWN Compile (rather than sharing one branch with digest)
// so a future digest-specific change cannot silently alter delay's output.
type delay struct{}

func (delay) Name() string                                       { return "delay" }
func (delay) DefaultControlValues() json.RawMessage              { return sleepControlValues() }
func (delay) ControlMap(cv json.RawMessage) map[string]string    { return flattenControlValues(cv) }
func (delay) ParseForm(form url.Values) (json.RawMessage, error) { return parseSleepForm(form) }
func (delay) Validate(cv json.RawMessage) error                  { _, err := sleepConfig(cv); return err }
func (delay) EditorPartial() string                              { return "wf-step-fields-delay" }
func (delay) Compile(step domain.Step) (engine.StepDef, error)   { return sleepStepDef(step) }

// digest is the digest step type. Today it compiles to the SAME engine sleep as
// delay, but it carries its OWN Compile so the two can diverge later without
// contaminating each other (the structural reason this refactor splits them).
type digest struct{}

func (digest) Name() string                                       { return "digest" }
func (digest) DefaultControlValues() json.RawMessage              { return sleepControlValues() }
func (digest) ControlMap(cv json.RawMessage) map[string]string    { return flattenControlValues(cv) }
func (digest) ParseForm(form url.Values) (json.RawMessage, error) { return parseSleepForm(form) }
func (digest) Validate(cv json.RawMessage) error                  { _, err := sleepConfig(cv); return err }
func (digest) EditorPartial() string                              { return "wf-step-fields-digest" }
func (digest) Compile(step domain.Step) (engine.StepDef, error)   { return sleepStepDef(step) }
