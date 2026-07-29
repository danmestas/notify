package steptype_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/steptype"
)

// notify's cron parser accepted a strict SUPERSET of what the dagnats engine
// understands: an optional seconds field (6-field form) and @descriptor
// shorthands. That superset is why a scheduled digest cannot compile to the
// engine's native cron sleep -- an expression notify happily saved could be one
// the engine cannot parse (#258).
//
// The grammar is now the intersection: exactly five fields, no descriptors. One
// parser still serves both save-time validation and run-time scheduling, so the
// two cannot drift.

// TestParseCron_RejectsSecondsField pins the narrowing. A 6-field expression was
// previously valid and silently gave second-granularity digests the engine could
// never reproduce.
func TestParseCron_RejectsSecondsField(t *testing.T) {
	for _, spec := range []string{
		"*/30 * * * * *", // every 30 seconds
		"0 0 * * * *",    // hourly, with explicit seconds
	} {
		if _, err := steptype.ParseCron(spec); err == nil {
			t.Errorf("ParseCron(%q) was accepted; the seconds field must be rejected", spec)
		}
	}
}

// TestParseCron_RejectsDescriptors pins the other half. @hourly and friends are
// expressive but have no equivalent in the engine's grammar.
func TestParseCron_RejectsDescriptors(t *testing.T) {
	for _, spec := range []string{"@hourly", "@daily", "@weekly", "@every 1h"} {
		if _, err := steptype.ParseCron(spec); err == nil {
			t.Errorf("ParseCron(%q) was accepted; descriptors must be rejected", spec)
		}
	}
}

// TestParseCron_AcceptsFiveField keeps the narrowing from over-reaching: the
// ordinary forms operators actually write must still work.
func TestParseCron_AcceptsFiveField(t *testing.T) {
	for _, spec := range []string{
		"0 * * * *",    // hourly
		"0 9 * * *",    // 09:00 daily
		"0 9 * * MON",  // 09:00 Mondays
		"*/15 * * * *", // every 15 minutes
		"30 8 1 * *",   // 08:30 on the 1st
	} {
		if _, err := steptype.ParseCron(spec); err != nil {
			t.Errorf("ParseCron(%q) rejected a valid 5-field expression: %v", spec, err)
		}
	}
}

// TestDigestValidate_RejectsLooseCron proves the narrowing reaches the authoring
// gate, which is where an operator finds out -- rather than at dispatch, where a
// digest would simply fail to fire.
func TestDigestValidate_RejectsLooseCron(t *testing.T) {
	dg := steptype.Default().Get("digest")

	loose := json.RawMessage(`{"type":"scheduled","cron":"@hourly"}`)
	err := dg.Validate(loose)
	if err == nil {
		t.Fatal("Validate accepted a descriptor cron")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cron") {
		t.Errorf("error should mention cron, got: %v", err)
	}

	sixField := json.RawMessage(`{"type":"scheduled","cron":"0 0 * * * *"}`)
	if err := dg.Validate(sixField); err == nil {
		t.Fatal("Validate accepted a 6-field cron")
	}

	ok := json.RawMessage(`{"type":"scheduled","cron":"0 9 * * MON"}`)
	if err := dg.Validate(ok); err != nil {
		t.Errorf("Validate rejected a valid 5-field cron: %v", err)
	}
}
