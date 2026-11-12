package smskeyword

import "testing"

func TestClassify(t *testing.T) {
	for body, want := range map[string]Kind{
		"STOP": OptOut, " stop ": OptOut, "StopAll": OptOut, "unsubscribe": OptOut, "CANCEL": OptOut, "end": OptOut, "Quit": OptOut,
		"START": OptIn, "yes": OptIn, "Unstop": OptIn,
		"HELP": Help, " info\n": Help,
		"":                     None,
		"please stop by later": None,
		"stop.":                None,
	} {
		if got := Classify(body); got != want {
			t.Errorf("Classify(%q) = %v, want %v", body, got, want)
		}
	}
	if Normalize("  sToP ") != "STOP" {
		t.Errorf("Normalize = %q", Normalize("  sToP "))
	}
}
