// Package timestamp defines the single wire format notify uses for timestamp
// columns stored as TEXT.
//
// Timestamps are persisted as strings and then compared as strings -- keyset
// pagination does `created_at < ?`, and every listing does `ORDER BY created_at`.
// That only yields chronological order if the encoding is order-preserving under
// byte comparison, which time.RFC3339Nano is NOT: it omits the fractional part
// entirely when it is zero, so a whole-second value sorts AFTER every sub-second
// value in the same second ('Z' > '.'):
//
//	RFC3339Nano, byte order:  ...T10:00:00.000000001Z  <  ...T10:00:00Z
//	true chronological order: ...T10:00:00Z            <  ...T10:00:00.000000001Z
//
// Layout pads the fraction to a fixed nine digits, which makes byte order and
// chronological order agree for all values. Writers must use Now/Format rather
// than formatting inline, so the property holds for every column by construction.
package timestamp

import "time"

// Layout is RFC 3339 with a fixed-width nine-digit fraction. The `.000000000`
// form pads with zeros; RFC3339Nano's `.999999999` form trims them, which is
// precisely the ordering bug this package exists to prevent.
const Layout = "2006-01-02T15:04:05.000000000Z07:00"

// Now returns the current UTC instant in Layout. This is the only way new rows
// should get a timestamp.
func Now() string {
	return Format(time.Now())
}

// Format renders t in Layout, normalising to UTC first so stored values share a
// single offset -- byte comparison across mixed offsets would be meaningless
// even with a fixed-width fraction.
func Format(t time.Time) string {
	return t.UTC().Format(Layout)
}

// Parse reads a stored timestamp. It accepts both Layout and the legacy
// variable-width RFC3339Nano encoding, since rows written before the format was
// pinned remain in the database; RFC3339Nano parsing is tolerant of any number
// of fractional digits, so it covers both.
func Parse(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}
