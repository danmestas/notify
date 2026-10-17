package steptype_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// TestCompile_ScheduledDelayIsNativeCronSleep: a scheduled delay that cron can
// express compiles to the engine's cron sleep, resolved in the recipient's
// timezone, so no notify worker is held for the wait.
func TestCompile_ScheduledDelayIsNativeCronSleep(t *testing.T) {
	cases := []struct{ cv, want string }{
		{`{"type":"scheduled","atTime":"09:30"}`, `{"cron":"30 9 * * *","cron_timezone_input_path":"subscriber.timezone"}`},
		{`{"type":"scheduled","atTime":"18:05","weekDays":["FRI","MON"]}`, `{"cron":"5 18 * * 1,5","cron_timezone_input_path":"subscriber.timezone"}`},
		{`{"type":"scheduled","atTime":"00:00","monthDays":[15,1]}`, `{"cron":"0 0 1,15 * *","cron_timezone_input_path":"subscriber.timezone"}`},
		{`{"type":"scheduled","atTime":"07:00","weekDays":["SUN","SAT"]}`, `{"cron":"0 7 * * 0,6","cron_timezone_input_path":"subscriber.timezone"}`},
	}
	for _, c := range cases {
		def, err := steptype.Default().Get("delay").Compile(domain.Step{StepID: "d", Type: "delay", ControlValues: json.RawMessage(c.cv)})
		if err != nil {
			t.Fatalf("%s: %v", c.cv, err)
		}
		if def.Type != "sleep" || def.Task != "" || def.Retry != nil || string(def.Config) != c.want {
			t.Fatalf("%s: def = %+v config %s, want a sleep with %s", c.cv, def, def.Config, c.want)
		}
	}
}

// TestScheduledCron_MatchesTheCalendar: for every schedule cron can express,
// its occurrences over a year are exactly the days the calendar fires on.
// The engine runs the cron; this proves the translation, using the calendar's
// own next-fire function as the reference.
func TestScheduledCron_MatchesTheCalendar(t *testing.T) {
	cfgs := []steptype.ScheduledConfig{
		{AtTime: "09:30"},
		{AtTime: "18:05", WeekDays: []string{"MON", "FRI"}},
		{AtTime: "00:00", MonthDays: []int{1, 15, 31}},
		{AtTime: "23:59", WeekDays: []string{"SUN"}},
	}
	for _, cfg := range cfgs {
		expr, ok := steptype.ScheduledCron(cfg)
		if !ok {
			t.Fatalf("%+v: not expressible", cfg)
		}
		sch, err := steptype.ParseCron(expr)
		if err != nil {
			t.Fatalf("%+v: cron %q: %v", cfg, expr, err)
		}
		now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 60; i++ {
			want, err := steptype.NextScheduledFireIn(cfg, now, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if got := sch.Next(now); !got.Equal(want) {
				t.Fatalf("%+v (%s) after %s: cron %s, calendar %s", cfg, expr, now, got, want)
			}
			now = want
		}
	}
}

func TestScheduledCron_BothFiltersIsNotExpressible(t *testing.T) {
	if _, ok := steptype.ScheduledCron(steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"MON"}, MonthDays: []int{1}}); ok {
		t.Fatal("week days and month days together have no cron equivalent")
	}
}
