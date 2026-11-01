package steptype_test

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// compileDelay is a tiny helper to compile a delay step's controlValues.
func compileDelay(t *testing.T, cv string) (domain.Step, error) {
	t.Helper()
	step := domain.Step{StepID: "s", Type: "delay", ControlValues: json.RawMessage(cv)}
	_, err := steptype.Default().Get("delay").Compile(step)
	return step, err
}

// TestCompile_DelayWeeks proves a regular delay with unit "weeks" compiles to
// the native sleep StepDef with duration N*7*24h in nanoseconds.
func TestCompile_DelayWeeks(t *testing.T) {
	def, err := steptype.Default().Get("delay").Compile(domain.Step{
		StepID: "s", Type: "delay", ControlValues: json.RawMessage(`{"amount":2,"unit":"weeks"}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.Type != dag.StepTypeSleep || string(def.Config) != `{"duration":1209600000000000}` {
		t.Fatalf("delay weeks def = %+v, want sleep 1209600000000000", def)
	}
}

// TestCompile_DelayMonths proves a regular delay with unit "months" compiles to
// the native sleep StepDef with duration N*30*24h (30-day approximation).
func TestCompile_DelayMonths(t *testing.T) {
	def, err := steptype.Default().Get("delay").Compile(domain.Step{
		StepID: "s", Type: "delay", ControlValues: json.RawMessage(`{"amount":1,"unit":"months"}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.Type != dag.StepTypeSleep || string(def.Config) != `{"duration":2592000000000000}` {
		t.Fatalf("delay months def = %+v, want sleep 2592000000000000", def)
	}
}

// TestCompile_DelayScheduled proves a scheduled delay naming both week days and
// month days compiles to a normal delay.scheduled task with a retry policy and
// no static Config: cron cannot say "both", so the handler computes the wait.
func TestCompile_DelayScheduled(t *testing.T) {
	def, err := steptype.Default().Get("delay").Compile(domain.Step{
		StepID: "d", Type: "delay", ControlValues: json.RawMessage(`{"type":"scheduled","atTime":"09:00","weekDays":["MON","FRI"],"monthDays":[1]}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.ID != "d" || def.Type != dag.StepTypeNormal || def.Task != "notify.delay.scheduled" {
		t.Fatalf("scheduled delay def = %+v, want {ID:d Type:normal Task:delay.scheduled}", def)
	}
	if def.Config != nil {
		t.Fatalf("scheduled delay Config = %s, want nil", def.Config)
	}
	if def.Retry == nil || def.Retry.MaxAttempts < 2 {
		t.Fatalf("scheduled delay Retry = %+v, want MaxAttempts >= 2", def.Retry)
	}
}

// TestCompile_DelayDynamic proves a dynamic delay with no declared kind (a
// step stored before dynamicKind existed) still compiles to the delay.dynamic
// task with a retry policy and no static Config: it cannot be resolved at
// trigger time, so it keeps the worker path that fails it at dispatch.
func TestCompile_DelayDynamic(t *testing.T) {
	def, err := steptype.Default().Get("delay").Compile(domain.Step{
		StepID: "d", Type: "delay", ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.sendAt"}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.ID != "d" || def.Type != dag.StepTypeNormal || def.Task != "notify.delay.dynamic" {
		t.Fatalf("dynamic delay def = %+v, want {ID:d Type:normal Task:delay.dynamic}", def)
	}
	if def.Config != nil {
		t.Fatalf("dynamic delay Config = %s, want nil", def.Config)
	}
	if def.Retry == nil || def.Retry.MaxAttempts < 2 {
		t.Fatalf("dynamic delay Retry = %+v, want MaxAttempts >= 2", def.Retry)
	}
}

// TestDelayRetryPolicy_SurvivesRestarts proves the delay retry cap is large
// enough that routine deploys cannot exhaust it before a pending wait fires.
// A scheduled wait that spans weeks or months can easily outlive many restart
// cycles; 3 attempts would dead-letter it after the third deploy. The cap
// must be >= 100 (production target: 1000).
func TestDelayRetryPolicy_SurvivesRestarts(t *testing.T) {
	for _, cv := range []string{
		`{"type":"scheduled","atTime":"09:00","weekDays":["MON"],"monthDays":[1]}`,
		`{"type":"dynamic","dynamicKey":"payload.sendAt"}`,
	} {
		def, err := steptype.Default().Get("delay").Compile(domain.Step{
			StepID: "d", Type: "delay", ControlValues: json.RawMessage(cv),
		})
		if err != nil {
			t.Fatalf("compile %s: %v", cv, err)
		}
		if def.Retry == nil || def.Retry.MaxAttempts < 100 {
			t.Fatalf("delay Retry = %+v for cv %s: MaxAttempts must be >= 100 to survive routine deploys", def.Retry, cv)
		}
	}
}

// TestCompile_DelayDynamic_EmptyKey_Errors proves an empty dynamicKey fails
// compilation (authoring maps it to ErrCompileFailure / HTTP 422).
func TestCompile_DelayDynamic_EmptyKey_Errors(t *testing.T) {
	if _, err := compileDelay(t, `{"type":"dynamic","dynamicKey":""}`); err == nil {
		t.Fatal("compile empty dynamicKey: want error, got nil")
	}
}

// TestCompile_DelayScheduled_BadConfig_Errors proves an invalid calendar (bad
// atTime, unknown weekday, out-of-range monthDay) fails compilation.
func TestCompile_DelayScheduled_BadConfig_Errors(t *testing.T) {
	cases := []string{
		`{"type":"scheduled"}`,
		`{"type":"scheduled","atTime":"25:00","weekDays":["MON"]}`,
		`{"type":"scheduled","atTime":"09:00","weekDays":["FUNDAY"]}`,
		`{"type":"scheduled","atTime":"09:00","monthDays":[32]}`,
	}
	for _, cv := range cases {
		if _, err := compileDelay(t, cv); err == nil {
			t.Errorf("compile %s: want error, got nil", cv)
		}
	}
}

// TestCompile_DelayRegular_BackwardCompat proves a legacy {amount,unit} blob
// (no type field) still compiles to a sleep StepDef with no retry policy — the
// regular path is byte-identical to before this unit.
func TestCompile_DelayRegular_BackwardCompat(t *testing.T) {
	def, err := steptype.Default().Get("delay").Compile(domain.Step{
		StepID: "s", Type: "delay", ControlValues: json.RawMessage(`{"amount":3,"unit":"minutes"}`),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if def.Type != dag.StepTypeSleep || string(def.Config) != `{"duration":180000000000}` || def.Retry != nil {
		t.Fatalf("regular delay def = %+v, want sleep 180000000000 no-retry", def)
	}
}

// TestNextScheduledFire is the table-driven next-occurrence proof, covering
// same-day future, today-passed-roll-to-next-week, cross-week, daily (no
// filter), monthDays, and a weekday intersection. 2026-06-29 is a Monday.
func TestNextScheduledFire(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name string
		now  time.Time
		cfg  steptype.ScheduledConfig
		want time.Time
	}{
		{
			name: "today still future",
			now:  time.Date(2026, 6, 29, 8, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"MON"}},
			want: time.Date(2026, 6, 29, 9, 0, 0, 0, utc),
		},
		{
			name: "today passed rolls to next monday",
			now:  time.Date(2026, 6, 29, 10, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"MON"}},
			want: time.Date(2026, 7, 6, 9, 0, 0, 0, utc),
		},
		{
			name: "cross-week to friday",
			now:  time.Date(2026, 6, 29, 10, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"FRI"}},
			want: time.Date(2026, 7, 3, 9, 0, 0, 0, utc),
		},
		{
			name: "wrap from friday back to monday",
			now:  time.Date(2026, 7, 3, 10, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"MON"}},
			want: time.Date(2026, 7, 6, 9, 0, 0, 0, utc),
		},
		{
			name: "daily no filter",
			now:  time.Date(2026, 6, 29, 10, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00"},
			want: time.Date(2026, 6, 30, 9, 0, 0, 0, utc),
		},
		{
			name: "month day first",
			now:  time.Date(2026, 6, 29, 10, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00", MonthDays: []int{1}},
			want: time.Date(2026, 7, 1, 9, 0, 0, 0, utc),
		},
		{
			name: "earliest of two weekdays",
			now:  time.Date(2026, 6, 29, 10, 0, 0, 0, utc),
			cfg:  steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"MON", "FRI"}},
			want: time.Date(2026, 7, 3, 9, 0, 0, 0, utc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steptype.NextScheduledFire(tc.cfg, tc.now)
			if err != nil {
				t.Fatalf("NextScheduledFire: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("NextScheduledFire = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestParseScheduled_Invalid proves the shared validator rejects malformed
// calendar configs.
func TestParseScheduled_Invalid(t *testing.T) {
	cases := []steptype.ScheduledConfig{
		{AtTime: ""},
		{AtTime: "25:00"},
		{AtTime: "noon"},
		{AtTime: "09:00", WeekDays: []string{"FUNDAY"}},
		{AtTime: "09:00", MonthDays: []int{0}},
		{AtTime: "09:00", MonthDays: []int{32}},
	}
	for _, cfg := range cases {
		if _, err := steptype.ParseScheduled(cfg); err == nil {
			t.Errorf("ParseScheduled(%+v): want error, got nil", cfg)
		}
	}
}

// TestDelay_ParseForm_Regular proves the regular path round-trips amount+unit
// (including the new weeks/months units) with no type field, matching the
// historical shape so the default seed stays valid.
func TestDelay_ParseForm_Regular(t *testing.T) {
	got, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"amount": {"2"}, "unit": {"weeks"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if string(got) != `{"amount":2,"unit":"weeks"}` {
		t.Fatalf("ParseForm regular = %s", got)
	}
}

// TestDelay_ParseForm_Scheduled builds scheduled controlValues from the editor
// time-picker + weekday checkboxes and rejects an empty calendar.
func TestDelay_ParseForm_Scheduled(t *testing.T) {
	got, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"type": {"scheduled"}, "atTime": {"09:00"}, "weekDays": {"MON", "FRI"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if string(got) != `{"atTime":"09:00","type":"scheduled","weekDays":["MON","FRI"]}` {
		t.Fatalf("ParseForm scheduled = %s", got)
	}
	if _, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"type": {"scheduled"}, "atTime": {"bad"},
	}); err == nil {
		t.Fatal("ParseForm bad atTime: want error, got nil")
	}
}

// TestDelay_ParseForm_Dynamic builds dynamic controlValues and rejects an empty
// dynamicKey.
func TestDelay_ParseForm_Dynamic(t *testing.T) {
	got, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"type": {"dynamic"}, "dynamicKey": {"payload.sendAt"},
		// dynamicKind is required as of #258: the step declares how the payload
		// value is interpreted rather than the dispatcher sniffing its shape.
		"dynamicKind": {"instant"},
	})
	if err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if string(got) != `{"dynamicKey":"payload.sendAt","dynamicKind":"instant","type":"dynamic"}` {
		t.Fatalf("ParseForm dynamic = %s", got)
	}
	if _, err := steptype.Default().Get("delay").ParseForm(url.Values{
		"type": {"dynamic"}, "dynamicKey": {""},
	}); err == nil {
		t.Fatal("ParseForm empty dynamicKey: want error, got nil")
	}
}

// TestDelay_ControlMap_SeedsTypeAndExpandsDays proves the editor pre-fill map
// defaults the type to "regular" and expands weekDays/monthDays for the
// scheduled checkboxes.
func TestDelay_ControlMap_SeedsTypeAndExpandsDays(t *testing.T) {
	reg := steptype.Default().Get("delay").ControlMap(json.RawMessage(`{"amount":5,"unit":"hours"}`))
	if reg["type"] != "regular" {
		t.Fatalf("regular ControlMap type = %q, want regular", reg["type"])
	}
	sched := steptype.Default().Get("delay").ControlMap(json.RawMessage(`{"type":"scheduled","atTime":"09:00","weekDays":["MON","FRI"],"monthDays":[1,15]}`))
	if sched["type"] != "scheduled" || sched["atTime"] != "09:00" {
		t.Fatalf("scheduled ControlMap = %+v", sched)
	}
	if sched["weekDay_MON"] != "true" || sched["weekDay_FRI"] != "true" || sched["weekDay_TUE"] != "" {
		t.Fatalf("scheduled ControlMap weekday flags = %+v", sched)
	}
	if sched["monthDays"] != "1,15" {
		t.Fatalf("scheduled ControlMap monthDays = %q, want 1,15", sched["monthDays"])
	}
}
