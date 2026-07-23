package timestamp_test

import (
	"sort"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/timestamp"
)

// orderingProbe is the set that exposes the defect: a whole-second instant plus
// several sub-second instants inside that same second. Under RFC3339Nano the
// whole-second value loses its fraction and sorts last instead of first.
func orderingProbe() []time.Time {
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	return []time.Time{
		base,
		base.Add(1 * time.Nanosecond),
		base.Add(50 * time.Millisecond),
		base.Add(500 * time.Millisecond),
		base.Add(999999999 * time.Nanosecond),
		base.Add(1 * time.Second),
	}
}

// TestFormat_ByteOrderMatchesChronologicalOrder is the property the store
// depends on: sorting the encoded strings must produce the same sequence as
// sorting the instants. Keyset pagination (`created_at < ?`) and every
// `ORDER BY created_at` are only correct if this holds.
func TestFormat_ByteOrderMatchesChronologicalOrder(t *testing.T) {
	instants := orderingProbe()

	chronological := make([]time.Time, len(instants))
	copy(chronological, instants)
	sort.Slice(chronological, func(i, j int) bool { return chronological[i].Before(chronological[j]) })

	wantOrder := make([]string, len(chronological))
	for i, inst := range chronological {
		wantOrder[i] = timestamp.Format(inst)
	}

	gotOrder := make([]string, len(instants))
	for i, inst := range instants {
		gotOrder[i] = timestamp.Format(inst)
	}
	sort.Strings(gotOrder)

	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("byte order != chronological order at %d:\n  byte:   %v\n  chrono: %v", i, gotOrder, wantOrder)
		}
	}
}

// TestFormat_IsFixedWidth pins the invariant that makes the ordering property
// hold. Any value whose length differs has a truncated fraction and reintroduces
// the bug.
func TestFormat_IsFixedWidth(t *testing.T) {
	const want = len("2006-01-02T15:04:05.000000000Z")

	for _, inst := range orderingProbe() {
		got := timestamp.Format(inst)
		if len(got) != want {
			t.Errorf("Format(%s) = %q (len %d), want fixed length %d", inst, got, len(got), want)
		}
	}
}

// TestFormat_RFC3339NanoWouldFail documents why this package exists rather than
// a bare time.RFC3339Nano call. If this ever stops failing, the standard-library
// behaviour changed and the indirection can be revisited.
func TestFormat_RFC3339NanoWouldFail(t *testing.T) {
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

	wholeSecond := base.UTC().Format(time.RFC3339Nano)
	subSecond := base.Add(1 * time.Nanosecond).UTC().Format(time.RFC3339Nano)

	if wholeSecond < subSecond {
		t.Fatalf("RFC3339Nano now orders correctly (%q < %q) -- revisit whether Layout is still needed", wholeSecond, subSecond)
	}
	if got := timestamp.Format(base); got >= timestamp.Format(base.Add(1*time.Nanosecond)) {
		t.Fatalf("Layout failed to fix the ordering that RFC3339Nano gets wrong: %q", got)
	}
}

// TestFormat_NormalisesToUTC guards the other half of comparability: two rows
// written at the same instant in different zones must encode identically.
func TestFormat_NormalisesToUTC(t *testing.T) {
	inst := time.Date(2026, 7, 22, 10, 0, 0, 123456789, time.UTC)
	elsewhere := inst.In(time.FixedZone("UTC+5", 5*60*60))

	if got, want := timestamp.Format(elsewhere), timestamp.Format(inst); got != want {
		t.Fatalf("Format is offset-sensitive: %q != %q", got, want)
	}
}

// TestParse_AcceptsLegacyAndFixedWidth keeps historical rows readable -- the
// database still holds variable-width values written before Layout was pinned.
func TestParse_AcceptsLegacyAndFixedWidth(t *testing.T) {
	want := time.Date(2026, 7, 22, 10, 0, 0, 500000000, time.UTC)

	for _, encoded := range []string{
		"2026-07-22T10:00:00.5Z",         // legacy RFC3339Nano
		"2026-07-22T10:00:00.500000000Z", // fixed width
	} {
		got, err := timestamp.Parse(encoded)
		if err != nil {
			t.Fatalf("Parse(%q): %v", encoded, err)
		}
		if !got.Equal(want) {
			t.Errorf("Parse(%q) = %v, want %v", encoded, got, want)
		}
	}
}

// TestNow_RoundTrips checks the writer path end to end.
func TestNow_RoundTrips(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Second)

	got, err := timestamp.Parse(timestamp.Now())
	if err != nil {
		t.Fatalf("Parse(Now()): %v", err)
	}
	if got.Before(before) {
		t.Errorf("Now() = %v, which predates the call site (%v)", got, before)
	}
}
