package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// cronParser parses digest cron expressions. SecondOptional accepts BOTH the
// 5-field standard form ("0 * * * *", minute granularity) and the 6-field
// with-seconds form ("* * * * * *", second granularity); Descriptor enables
// "@hourly" and friends. The same parser validates at compile/save time and
// schedules at run time so the two can never drift.
var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// ParseCron parses a digest cron expression into a schedule. It is the single
// source of cron semantics shared by the digest step type (compile/save-time
// validation) and the delivery digest.wait handler (run-time scheduling).
func ParseCron(spec string) (cron.Schedule, error) { return cronParser.Parse(spec) }

// digestControlValues is the persisted shape of a digest step's controlValues.
// Regular windows carry amount+unit (compiled to a sleep); scheduled windows
// carry a cron expression (compiled to a digest.wait task). digestKey is an
// optional dot-path resolved at trigger time to group events into separate
// digests; it never affects the compiled StepDef.
type digestControlValues struct {
	Type      string `json:"type"`
	Amount    int64  `json:"amount"`
	Unit      string `json:"unit"`
	Cron      string `json:"cron"`
	DigestKey string `json:"digestKey"`
}

// digestType reports the digest window type ("regular" or "scheduled"),
// defaulting absent/empty to "regular" so legacy {amount,unit} blobs (written
// before the scheduled window existed) keep compiling to a sleep step.
func digestType(controlValues json.RawMessage) (digestControlValues, error) {
	var cv digestControlValues
	if len(controlValues) > 0 {
		if err := json.Unmarshal(controlValues, &cv); err != nil {
			return cv, fmt.Errorf("unmarshal digest controlValues: %w", err)
		}
	}
	if cv.Type == "" {
		cv.Type = "regular"
	}
	return cv, nil
}

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

// digest is the digest step type. A regular window (type absent or "regular")
// compiles to the SAME engine sleep as delay; a scheduled window
// (type "scheduled" + cron) compiles to a normal "digest.wait" task whose wait
// duration is computed per-run by the delivery handler.
type digest struct{}

func (digest) Name() string                                    { return "digest" }
func (digest) ControlMap(cv json.RawMessage) map[string]string { return flattenControlValues(cv) }
func (digest) EditorPartial() string                           { return "wf-step-fields-digest" }

// DefaultControlValues seeds a regular 1-minute window with an empty digestKey.
func (digest) DefaultControlValues() json.RawMessage {
	return json.RawMessage(`{"type":"regular","amount":1,"unit":"minutes","digestKey":""}`)
}

// ParseForm builds digest controlValues from the editor's type/digestKey/cron
// and (for regular) amount/unit fields. A scheduled window requires a parseable
// cron expression; a regular window requires a positive amount.
func (digest) ParseForm(form url.Values) (json.RawMessage, error) {
	typ := formField(form, "type")
	if typ == "" {
		typ = "regular"
	}
	digestKey := formField(form, "digestKey")
	switch typ {
	case "regular":
		amount, err := strconv.Atoi(formField(form, "amount"))
		if err != nil || amount < 1 {
			return nil, fmt.Errorf("amount must be a positive whole number")
		}
		unit := formField(form, "unit")
		if unit == "" {
			unit = "minutes"
		}
		return marshalControlValues(map[string]any{
			"type": "regular", "amount": amount, "unit": unit, "digestKey": digestKey,
		})
	case "scheduled":
		cronExpr := formField(form, "cron")
		if _, err := ParseCron(cronExpr); err != nil {
			return nil, fmt.Errorf("cron must be a valid cron expression")
		}
		return marshalControlValues(map[string]any{
			"type": "scheduled", "cron": cronExpr, "digestKey": digestKey,
		})
	default:
		return nil, fmt.Errorf("unknown digest type %q", typ)
	}
}

// Validate accepts a regular window with a positive amount or a scheduled
// window with a parseable cron expression.
func (digest) Validate(cv json.RawMessage) error {
	dcv, err := digestType(cv)
	if err != nil {
		return err
	}
	if dcv.Type == "scheduled" {
		_, perr := ParseCron(dcv.Cron)
		return perr
	}
	_, err = sleepConfig(cv)
	return err
}

// Compile turns a regular window into a sleep StepDef (byte-identical to delay)
// and a scheduled window into a normal digest.wait task. An invalid scheduled
// cron returns an error so authoring rejects the workflow with HTTP 422.
func (digest) Compile(step domain.Step) (engine.StepDef, error) {
	dcv, err := digestType(step.ControlValues)
	if err != nil {
		return engine.StepDef{}, err
	}
	if dcv.Type == "scheduled" {
		if _, perr := ParseCron(dcv.Cron); perr != nil {
			return engine.StepDef{}, fmt.Errorf("invalid digest cron %q: %w", dcv.Cron, perr)
		}
		return engine.StepDef{ID: step.StepID, Type: "normal", Task: "digest.wait"}, nil
	}
	return sleepStepDef(step)
}
