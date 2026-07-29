package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// cronParser parses digest cron expressions. SecondOptional accepts BOTH the
// exactly the 5-field standard form ("0 * * * *", minute granularity).
//
// Deliberately NOT a superset of that. The parser previously also accepted a
// 6-field with-seconds form and "@hourly"-style descriptors, which meant notify
// would happily save a digest schedule the dagnats engine cannot parse -- and
// that gap is precisely what stops a scheduled digest compiling to the engine's
// native cron sleep instead of a goroutine-blocking handler (#258). Narrowing to
// the intersection makes every saved digest expressible by the engine.
//
// The same parser validates at compile/save time and schedules at run time, so
// the two can never drift; narrowing it narrows both together, by design.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// ParseCron parses a digest cron expression into a schedule. It is the single
// source of cron semantics shared by the digest step type (compile/save-time
// validation) and the delivery digest.wait handler (run-time scheduling).
func ParseCron(spec string) (cron.Schedule, error) {
	sch, err := cronParser.Parse(spec)
	if err != nil {
		// The underlying parser's errors ("parser does not accept descriptors")
		// describe its own configuration, not the operator's problem. Name the
		// field and the accepted grammar so the message is actionable from the
		// editor without reading this source.
		return nil, fmt.Errorf("invalid digest cron %q: %w (use a 5-field expression such as \"0 9 * * MON\"; seconds and @descriptors are not supported)", spec, err)
	}
	return sch, nil
}

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

// UnitDuration maps a delay/digest unit token to its time.Duration. It is the
// single source of unit semantics shared by the compile-time sleep duration
// (sleepConfig) and the run-time dynamic-delay {amount,unit} resolver, so both
// honour weeks/months identically. "months" is a 30-day approximation (a
// calendar month is not a fixed duration); the editor labels it as such.
func UnitDuration(unit string) (time.Duration, error) {
	switch unit {
	case "", "seconds":
		return time.Second, nil
	case "minutes":
		return time.Minute, nil
	case "hours":
		return time.Hour, nil
	case "days":
		return 24 * time.Hour, nil
	case "weeks":
		return 7 * 24 * time.Hour, nil
	case "months":
		return 30 * 24 * time.Hour, nil // 30-day approximation
	default:
		return 0, fmt.Errorf("unknown delay unit %q", unit)
	}
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
	unit, err := UnitDuration(cv.Unit)
	if err != nil {
		return nil, err
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

// delayControlValues is the persisted shape of a delay step's controlValues
// across its three sub-types. A regular delay (type absent or "regular") carries
// amount+unit and compiles to the engine's native sleep. A scheduled delay
// carries a structured calendar (atTime + weekDays/monthDays) and a dynamic
// delay carries a dynamicKey dot-path; both compile to a normal task whose wait
// the delay worker computes per-run (the duration depends on wall-clock time or
// the trigger payload, so it cannot be baked into a static sleep StepDef).
type delayControlValues struct {
	Type       string   `json:"type"`
	AtTime     string   `json:"atTime"`
	WeekDays   []string `json:"weekDays"`
	MonthDays  []int    `json:"monthDays"`
	DynamicKey string   `json:"dynamicKey"`
	// DynamicKind declares how the value at DynamicKey is interpreted. It is
	// required for dynamic delays and exists so the interpretation is a property
	// of the AUTHORED STEP rather than of whatever a given trigger happened to
	// send. The dispatcher previously sniffed the value's shape per payload,
	// which meant one step could behave two ways -- and made these steps
	// impossible to compile to the engine's native sleep, because compile time
	// cannot know which shape will arrive (see #258).
	DynamicKind string `json:"dynamicKind"`
}

// The two interpretations a dynamic delay's payload value can carry.
const (
	// DynamicKindInstant: the value is an RFC3339 timestamp; wait until it.
	DynamicKindInstant = "instant"
	// DynamicKindRelative: the value is {amount,unit}; wait that long. Uses
	// notify's own unit vocabulary (including "months"), which is why this form
	// stays consumer-side rather than compiling to a native sleep.
	DynamicKindRelative = "relative"
)

// validDynamicKind reports whether k is a declared interpretation.
func validDynamicKind(k string) bool {
	return k == DynamicKindInstant || k == DynamicKindRelative
}

// delayType decodes a delay step's sub-type discriminator, defaulting an
// absent/empty type to "regular" so legacy {amount,unit} blobs keep compiling
// to a sleep.
func delayType(controlValues json.RawMessage) (delayControlValues, error) {
	var cv delayControlValues
	if len(controlValues) > 0 {
		if err := json.Unmarshal(controlValues, &cv); err != nil {
			return cv, fmt.Errorf("unmarshal delay controlValues: %w", err)
		}
	}
	if cv.Type == "" {
		cv.Type = "regular"
	}
	return cv, nil
}

// maxDelayResumeAttempts is the attempt cap on scheduled/dynamic delay tasks.
// It is intentionally large: these waits are always safely resumable (the
// handler recomputes the remaining duration from time.Now() on every dispatch),
// so the cap is a runaway backstop, NOT a wait-duration limit. A value of 3
// would dead-letter the task after just three process restarts — silently
// dropping the notification if the delay spans multiple deploys.
const maxDelayResumeAttempts = 1000

// delayRetryPolicy is attached to the scheduled/dynamic StepDef so dagnats
// requeues the task when its handler returns a RetryableError. The delay
// handlers return RetryableError on context cancellation (shutdown): the
// requeue re-dispatches the wait after restart, where a scheduled delay
// recomputes the next occurrence and an ISO-8601 dynamic delay recomputes the
// remaining duration. Without a RetryPolicy the requeue would never fire.
func delayRetryPolicy() *engine.RetryPolicy {
	return &engine.RetryPolicy{
		MaxAttempts:  maxDelayResumeAttempts,
		Strategy:     "fixed",
		InitialDelay: time.Second,
		MaxDelay:     time.Second,
	}
}

// delay is the delay step type. A regular delay compiles to an engine sleep the
// engine times itself; scheduled and dynamic delays compile to a normal task
// (delay.scheduled / delay.dynamic) handled by the delivery delay worker. It has
// its OWN Compile (rather than sharing one branch with digest) so the
// type-discriminator here cannot contaminate digest's output.
type delay struct{}

func (delay) Name() string                          { return "delay" }
func (delay) DisplayName() string                   { return "Delay" }
func (delay) DefaultControlValues() json.RawMessage { return sleepControlValues() }
func (delay) EditorPartial() string                 { return "wf-step-fields-delay" }

// ControlMap flattens controlValues for editor pre-fill and additionally seeds
// the type discriminator (default "regular") and expands the weekDays/monthDays
// arrays (which flattenControlValues drops) into editor-friendly keys: a
// "weekDay_MON" presence flag per selected day and a comma-joined "monthDays".
func (delay) ControlMap(cv json.RawMessage) map[string]string {
	m := flattenControlValues(cv)
	if m["type"] == "" {
		m["type"] = "regular"
	}
	var raw struct {
		WeekDays  []string `json:"weekDays"`
		MonthDays []int    `json:"monthDays"`
	}
	if len(cv) > 0 {
		_ = json.Unmarshal(cv, &raw)
	}
	for _, d := range raw.WeekDays {
		m["weekDay_"+strings.ToUpper(strings.TrimSpace(d))] = "true"
	}
	if len(raw.MonthDays) > 0 {
		parts := make([]string, len(raw.MonthDays))
		for i, d := range raw.MonthDays {
			parts[i] = strconv.Itoa(d)
		}
		m["monthDays"] = strings.Join(parts, ",")
	}
	return m
}

// ParseForm builds delay controlValues from the editor's type/amount/unit
// (regular), atTime/weekDays/monthDays (scheduled), or dynamicKey (dynamic)
// fields. A scheduled delay requires a valid calendar; a dynamic delay requires
// a non-empty dynamicKey.
func (delay) ParseForm(form url.Values) (json.RawMessage, error) {
	typ := formField(form, "type")
	if typ == "" {
		typ = "regular"
	}
	switch typ {
	case "regular":
		return parseSleepForm(form)
	case "scheduled":
		monthDays, err := parseMonthDays(formField(form, "monthDays"))
		if err != nil {
			return nil, err
		}
		cfg := ScheduledConfig{
			AtTime:    formField(form, "atTime"),
			WeekDays:  formWeekDays(form),
			MonthDays: monthDays,
		}
		if _, err := ParseScheduled(cfg); err != nil {
			return nil, err
		}
		out := map[string]any{"type": "scheduled", "atTime": cfg.AtTime}
		if len(cfg.WeekDays) > 0 {
			out["weekDays"] = cfg.WeekDays
		}
		if len(cfg.MonthDays) > 0 {
			out["monthDays"] = cfg.MonthDays
		}
		return marshalControlValues(out)
	case "dynamic":
		key := formField(form, "dynamicKey")
		if key == "" {
			return nil, fmt.Errorf("dynamic delay requires a dynamicKey")
		}
		kind := formField(form, "dynamicKind")
		if kind == "" {
			return nil, fmt.Errorf("dynamic delay requires a dynamicKind (%q or %q)", DynamicKindInstant, DynamicKindRelative)
		}
		if !validDynamicKind(kind) {
			return nil, fmt.Errorf("unknown dynamicKind %q (use %q or %q)", kind, DynamicKindInstant, DynamicKindRelative)
		}
		return marshalControlValues(map[string]any{
			"type": "dynamic", "dynamicKey": key, "dynamicKind": kind,
		})
	default:
		return nil, fmt.Errorf("unknown delay type %q", typ)
	}
}

// Validate accepts a regular delay with a positive amount, a scheduled delay
// with a valid calendar, or a dynamic delay with a non-empty dynamicKey.
func (delay) Validate(cv json.RawMessage) error {
	dcv, err := delayType(cv)
	if err != nil {
		return err
	}
	switch dcv.Type {
	case "scheduled":
		_, err := ParseScheduled(ScheduledConfig{AtTime: dcv.AtTime, WeekDays: dcv.WeekDays, MonthDays: dcv.MonthDays})
		return err
	case "dynamic":
		if dcv.DynamicKey == "" {
			return fmt.Errorf("dynamic delay requires a dynamicKey")
		}
		// Required, with no inferred default: an absent kind is precisely the
		// ambiguous state this contract removes, so it fails here rather than
		// being resolved by guesswork at dispatch.
		if dcv.DynamicKind == "" {
			return fmt.Errorf("dynamic delay requires a dynamicKind (%q or %q)", DynamicKindInstant, DynamicKindRelative)
		}
		if !validDynamicKind(dcv.DynamicKind) {
			return fmt.Errorf("unknown dynamicKind %q (use %q or %q)", dcv.DynamicKind, DynamicKindInstant, DynamicKindRelative)
		}
		return nil
	default:
		_, err := sleepConfig(cv)
		return err
	}
}

// Compile routes a regular delay to the native sleep StepDef and a
// scheduled/dynamic delay to a normal task. Invalid scheduled/dynamic config
// returns an error so authoring rejects the workflow with HTTP 422.
func (delay) Compile(step domain.Step) (engine.StepDef, error) {
	dcv, err := delayType(step.ControlValues)
	if err != nil {
		return engine.StepDef{}, err
	}
	switch dcv.Type {
	case "scheduled":
		if _, perr := ParseScheduled(ScheduledConfig{AtTime: dcv.AtTime, WeekDays: dcv.WeekDays, MonthDays: dcv.MonthDays}); perr != nil {
			return engine.StepDef{}, fmt.Errorf("invalid scheduled delay: %w", perr)
		}
		return engine.StepDef{ID: step.StepID, Type: "normal", Task: "delay.scheduled", Retry: delayRetryPolicy()}, nil
	case "dynamic":
		if dcv.DynamicKey == "" {
			return engine.StepDef{}, fmt.Errorf("dynamic delay requires a dynamicKey")
		}
		return engine.StepDef{ID: step.StepID, Type: "normal", Task: "delay.dynamic", Retry: delayRetryPolicy()}, nil
	default:
		return sleepStepDef(step)
	}
}

// formWeekDays reads the multi-valued weekDays checkbox field, normalising each
// token to upper-case and dropping blanks.
func formWeekDays(form url.Values) []string {
	var out []string
	for _, v := range form["weekDays"] {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseMonthDays parses the comma-separated monthDays field ("1,15") into a
// []int. Blank segments are ignored; a non-numeric segment is an error.
func parseMonthDays(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, seg := range strings.Split(s, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		n, err := strconv.Atoi(seg)
		if err != nil {
			return nil, fmt.Errorf("scheduled delay monthDays %q must be comma-separated whole numbers", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// digest is the digest step type. A regular window (type absent or "regular")
// compiles to the SAME engine sleep as delay; a scheduled window
// (type "scheduled" + cron) compiles to a normal "digest.wait" task whose wait
// duration is computed per-run by the delivery handler.
type digest struct{}

func (digest) Name() string                                    { return "digest" }
func (digest) DisplayName() string                             { return "Digest" }
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
		return engine.StepDef{ID: step.StepID, Type: "normal", Task: "digest.wait", Retry: delayRetryPolicy()}, nil
	}
	return sleepStepDef(step)
}
