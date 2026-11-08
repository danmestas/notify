package phonenum

import "testing"

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"+15551234567":      "+15551234567",
		"+1 (555) 123-4567": "+15551234567",
		"+44 20 7946 0958":  "+442079460958",
		" +49.30.1234567 ":  "+49301234567",
		"+123456789012345":  "+123456789012345",
		"+1234567":          "+1234567",
		"+1-555-123-4567 ":  "+15551234567",
		"+1 555 1234567":    "+15551234567",
	} {
		got, ok := Normalize(in)
		if !ok || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "5551234567", "+", "+0123456789", "+1234567890123456", "+1555abc4567", "++15551234567", "+123456"} {
		if got, ok := Normalize(in); ok {
			t.Errorf("Normalize(%q) = %q, want not ok", in, got)
		}
	}
}

func TestCanonical(t *testing.T) {
	if got := Canonical("+1 (555) 123-4567"); got != "+15551234567" {
		t.Errorf("Canonical(formatted) = %q", got)
	}
	// What cannot be normalised is kept as given, so no write is lost.
	if got := Canonical("555-1234 ext 9"); got != "555-1234 ext 9" {
		t.Errorf("Canonical(invalid) = %q, want it unchanged", got)
	}
}
