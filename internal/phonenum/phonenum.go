// Package phonenum normalises phone numbers to E.164. It is the one place
// notify decides what a phone number is: the store normalises every phone it
// writes through it, and phone verification accepts only what it accepts, so
// the same number written in two formats is the same number.
package phonenum

import "strings"

// Normalize returns number in E.164 form: a '+', a country code that does not
// start with 0, and 7 to 15 digits in all. Spaces (including no-break spaces),
// dots, dashes and parentheses are dropped. ok is false for anything else.
func Normalize(number string) (e164 string, ok bool) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(number) {
		switch {
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	out := b.String()
	if len(out) < 8 || len(out) > 16 || out[0] != '+' || out[1] == '0' {
		return "", false
	}
	return out, true
}

// Canonical returns number normalised when it can be, else as given.
func Canonical(number string) string {
	if n, ok := Normalize(number); ok {
		return n
	}
	return number
}
