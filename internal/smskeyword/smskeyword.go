// Package smskeyword names the SMS keywords notify acts on: the opt-out,
// opt-in and help words carriers and providers treat as standard.
package smskeyword

import "strings"

// Kind is what a keyword asks for.
type Kind int

const (
	// None: not a keyword.
	None Kind = iota
	// OptOut: STOP and its synonyms.
	OptOut
	// OptIn: START and its synonyms.
	OptIn
	// Help: HELP and INFO.
	Help
)

// Normalize is body as a candidate keyword: trimmed and upper-cased.
func Normalize(body string) string { return strings.ToUpper(strings.TrimSpace(body)) }

// Classify says which keyword body is. Only a whole-message match counts, so
// "please stop by" is just a text.
func Classify(body string) Kind {
	switch Normalize(body) {
	case "STOP", "STOPALL", "UNSUBSCRIBE", "CANCEL", "END", "QUIT":
		return OptOut
	case "START", "YES", "UNSTOP":
		return OptIn
	case "HELP", "INFO":
		return Help
	}
	return None
}
