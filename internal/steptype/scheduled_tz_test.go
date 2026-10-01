package steptype_test

import (
	"testing"
	"time"

	"github.com/danmestas/notify/internal/steptype"
)

// Scheduled delays computed their firing instant in UTC only, so "09:00" meant
// 09:00 UTC for every subscriber regardless of where they are. They now
// schedule in the subscriber's timezone: "send at 9am" means their 9am.
//
// The fallback behaviour is the load-bearing part: the subscriber timezone
// column has always existed and defaults to empty, so an absent or unparseable
// zone MUST resolve to UTC. That preserves today's behaviour exactly for every
// subscriber who has never had one set, and keeps a malformed profile value from
// killing a delivery.

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %q unavailable: %v", name, err)
	}
	return loc
}

// TestNextScheduledFireIn_UsesTheGivenZone is the core: the same config fires at
// the same WALL-CLOCK time in each zone, which is different absolute instants.
func TestNextScheduledFireIn_UsesTheGivenZone(t *testing.T) {
	cfg := steptype.ScheduledConfig{AtTime: "09:00"}
	// A moment when it is still before 09:00 everywhere involved.
	now := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)

	for _, zone := range []string{"UTC", "Europe/London", "America/New_York", "Asia/Tokyo"} {
		loc := mustLoad(t, zone)
		fire, err := steptype.NextScheduledFireIn(cfg, now, loc)
		if err != nil {
			t.Fatalf("%s: %v", zone, err)
		}
		got := fire.In(loc)
		if got.Hour() != 9 || got.Minute() != 0 {
			t.Errorf("%s: fires at %02d:%02d local, want 09:00", zone, got.Hour(), got.Minute())
		}
		if !fire.After(now) {
			t.Errorf("%s: fire %v is not after now %v", zone, fire, now)
		}
	}
}

// TestNextScheduledFireIn_ZonesDisagreeOnTheInstant proves the zone actually
// changes the answer -- if this passed with everything equal, the location
// argument would be decorative.
func TestNextScheduledFireIn_ZonesDisagreeOnTheInstant(t *testing.T) {
	cfg := steptype.ScheduledConfig{AtTime: "09:00"}
	now := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)

	utcFire, err := steptype.NextScheduledFireIn(cfg, now, time.UTC)
	if err != nil {
		t.Fatalf("utc: %v", err)
	}
	tokyoFire, err := steptype.NextScheduledFireIn(cfg, now, mustLoad(t, "Asia/Tokyo"))
	if err != nil {
		t.Fatalf("tokyo: %v", err)
	}
	if utcFire.Equal(tokyoFire) {
		t.Fatal("UTC and Tokyo produced the same instant for 09:00 local: the zone is being ignored")
	}
}

// TestNextScheduledFire_StillUTC pins backward compatibility: the original
// entry point must behave exactly as before, since every subscriber without a
// timezone continues to go through it.
func TestNextScheduledFire_StillUTC(t *testing.T) {
	cfg := steptype.ScheduledConfig{AtTime: "09:00", WeekDays: []string{"MON"}}
	now := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)

	legacy, err := steptype.NextScheduledFire(cfg, now)
	if err != nil {
		t.Fatalf("NextScheduledFire: %v", err)
	}
	explicit, err := steptype.NextScheduledFireIn(cfg, now, time.UTC)
	if err != nil {
		t.Fatalf("NextScheduledFireIn(UTC): %v", err)
	}
	if !legacy.Equal(explicit) {
		t.Errorf("NextScheduledFire = %v, NextScheduledFireIn(UTC) = %v: the UTC path changed", legacy, explicit)
	}
}

// TestNextScheduledFireIn_NilLocationFallsBackToUTC keeps a nil location from
// panicking deep in the scheduler. A delivery must not die because a location
// could not be resolved.
func TestNextScheduledFireIn_NilLocationFallsBackToUTC(t *testing.T) {
	cfg := steptype.ScheduledConfig{AtTime: "09:00"}
	now := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)

	got, err := steptype.NextScheduledFireIn(cfg, now, nil)
	if err != nil {
		t.Fatalf("nil location: %v", err)
	}
	want, _ := steptype.NextScheduledFireIn(cfg, now, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nil location = %v, want the UTC result %v", got, want)
	}
}

// TestNextScheduledFireIn_AcrossDST is the case a naive implementation gets
// wrong: 09:00 local stays 09:00 local either side of a DST transition, which
// means the UTC offset changes. Computing in UTC and converting cannot do this.
func TestNextScheduledFireIn_AcrossDST(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	cfg := steptype.ScheduledConfig{AtTime: "09:00"}

	// US DST began 2026-03-08. Before and after should both be 09:00 local.
	before := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)

	for _, now := range []time.Time{before, after} {
		fire, err := steptype.NextScheduledFireIn(cfg, now, loc)
		if err != nil {
			t.Fatalf("now=%v: %v", now, err)
		}
		if local := fire.In(loc); local.Hour() != 9 {
			t.Errorf("now=%v: fires at %02d:00 local, want 09:00 (DST not honoured)", now, local.Hour())
		}
	}
}

// TestResolveLocation covers the fallback contract at the boundary where a
// stored subscriber timezone becomes a location.
func TestResolveLocation(t *testing.T) {
	if got := steptype.ResolveLocation(""); got != time.UTC {
		t.Errorf("empty timezone = %v, want UTC (every subscriber without one must keep today's behaviour)", got)
	}
	if got := steptype.ResolveLocation("Not/AZone"); got != time.UTC {
		t.Errorf("unparseable timezone = %v, want UTC (a bad profile value must not kill a delivery)", got)
	}
	if got := steptype.ResolveLocation("Asia/Tokyo"); got == time.UTC {
		t.Error("a valid timezone resolved to UTC")
	}
}
