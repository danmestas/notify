package connectivity

import (
	"errors"

	"github.com/danmestas/notify/internal/smskeyword"
)

// ErrHelpTextIsKeyword refuses an SMS integration's help_text that is itself
// a keyword (STOP, START, HELP, ...): the reply to HELP would read as a
// command.
var ErrHelpTextIsKeyword = errors.New("connectivity: help_text cannot be an SMS keyword (STOP, START, HELP and their synonyms)")

// clearableCredentialFields are the credential fields an update clears with
// an empty value, because empty means "use the default". Every other field
// keeps its stored value when the update leaves it empty.
var clearableCredentialFields = map[string]bool{"help_text": true}

func checkHelpText(text string) error {
	if text != "" && smskeyword.Classify(text) != smskeyword.None {
		return ErrHelpTextIsKeyword
	}
	return nil
}
