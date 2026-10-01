package steptype

import (
	"fmt"
	"strings"
	"time"
)

// weekdayTokens maps the 3-letter calendar token used in a scheduled delay's
// weekDays list to a time.Weekday. The tokens are the persisted
// weekDays values.
var weekdayTokens = map[string]time.Weekday{
	"SUN": time.Sunday,
	"MON": time.Monday,
	"TUE": time.Tuesday,
	"WED": time.Wednesday,
	"THU": time.Thursday,
	"FRI": time.Friday,
	"SAT": time.Saturday,
}

// ScheduledConfig is the structured-calendar form of a scheduled delay:
// "wait until the next AtTime (HH:MM, 24-hour UTC) that falls on a matching
// WeekDay and/or MonthDay". It is the lighter alternative to a cron string
// (stdlib time arithmetic, no dependency) chosen for the delay step; digest
// keeps its cron form. The JSON tags match the persisted controlValues so a
// delay worker can unmarshal a step's controlValues straight into this type.
type ScheduledConfig struct {
	AtTime    string   `json:"atTime"`
	WeekDays  []string `json:"weekDays,omitempty"`
	MonthDays []int    `json:"monthDays,omitempty"`
}

// parsedSchedule is the validated, lookup-ready form of a ScheduledConfig.
type parsedSchedule struct {
	hour, minute int
	weekdays     map[time.Weekday]bool
	monthdays    map[int]bool
}

// ParseScheduled validates a scheduled delay's calendar config: AtTime is
// required and must be HH:MM (24-hour); every weekDays token must be a known
// 3-letter day; every monthDays entry must be 1..31. WeekDays and MonthDays are
// optional filters (both empty means "every day at AtTime"). It is the single
// source of schedule validity shared by compile/save-time validation and the
// run-time delay.scheduled handler, so the two can never drift.
func ParseScheduled(cfg ScheduledConfig) (parsedSchedule, error) {
	var ps parsedSchedule
	atTime := strings.TrimSpace(cfg.AtTime)
	if atTime == "" {
		return ps, fmt.Errorf("scheduled delay requires atTime (HH:MM, 24-hour UTC)")
	}
	t, err := time.Parse("15:04", atTime)
	if err != nil {
		return ps, fmt.Errorf("scheduled delay atTime %q must be HH:MM (24-hour UTC): %w", cfg.AtTime, err)
	}
	ps.hour, ps.minute = t.Hour(), t.Minute()

	ps.weekdays = make(map[time.Weekday]bool, len(cfg.WeekDays))
	for _, d := range cfg.WeekDays {
		wd, ok := weekdayTokens[strings.ToUpper(strings.TrimSpace(d))]
		if !ok {
			return ps, fmt.Errorf("scheduled delay weekDays contains unknown day %q (use MON..SUN)", d)
		}
		ps.weekdays[wd] = true
	}

	ps.monthdays = make(map[int]bool, len(cfg.MonthDays))
	for _, d := range cfg.MonthDays {
		if d < 1 || d > 31 {
			return ps, fmt.Errorf("scheduled delay monthDays contains out-of-range day %d (use 1..31)", d)
		}
		ps.monthdays[d] = true
	}
	return ps, nil
}

// NextScheduledFire returns the earliest instant strictly after now (evaluated
// in UTC) whose time-of-day equals AtTime and whose weekday and day-of-month
// satisfy the configured filters. An empty weekDays (or monthDays) filter
// matches every weekday (or day). With both filters set the match is their
// intersection (e.g. "the 1st when it is also a Monday").
func NextScheduledFire(cfg ScheduledConfig, now time.Time) (time.Time, error) {
	return NextScheduledFireIn(cfg, now, time.UTC)
}

// ResolveLocation turns a stored subscriber timezone into a location, falling
// back to UTC for both an empty and an unparseable value.
//
// Both fallbacks are load-bearing. The timezone column has existed since the
// first migration and defaults to empty, so empty means "never set" for a large
// share of subscribers -- resolving it to UTC preserves their scheduling exactly.
// And a malformed value is bad profile data, not a reason to fail a delivery:
// the send still goes, just on the previous UTC basis.
func ResolveLocation(timezone string) *time.Location {
	if timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// NextScheduledFireIn is NextScheduledFire computed in loc rather than UTC, so
// an authored "09:00" means 09:00 where the subscriber is.
//
// The candidate is built directly in loc rather than computed in UTC and
// converted, which is what makes it correct across a DST transition: 09:00 local
// stays 09:00 local while its UTC offset changes underneath.
//
// A nil loc resolves to UTC rather than panicking -- callers derive it from
// stored profile data, and a delivery must not die because a zone was missing.
func NextScheduledFireIn(cfg ScheduledConfig, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	ps, err := ParseScheduled(cfg)
	if err != nil {
		return time.Time{}, err
	}
	now = now.In(loc)
	cand := time.Date(now.Year(), now.Month(), now.Day(), ps.hour, ps.minute, 0, 0, loc)
	// A full leap year of days is an upper bound: any single weekday or
	// month-day filter (and their intersection) recurs within 366 days.
	for i := 0; i < 366; i++ {
		if cand.After(now) && ps.matchDay(cand) {
			return cand, nil
		}
		cand = cand.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("scheduled delay has no firing time within a year for %+v", cfg)
}

// matchDay reports whether t's weekday and day-of-month satisfy the filters.
func (ps parsedSchedule) matchDay(t time.Time) bool {
	if len(ps.weekdays) > 0 && !ps.weekdays[t.Weekday()] {
		return false
	}
	if len(ps.monthdays) > 0 && !ps.monthdays[t.Day()] {
		return false
	}
	return true
}
