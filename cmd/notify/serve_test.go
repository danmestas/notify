package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/dagnats/program"
)

// TestRunError_RelayUnavailableSaysWhatToDo: when nothing answers dagnats'
// program relay (a server older than notify needs, or none running its NATS
// API), serve tells the operator what to do rather than printing a timeout.
func TestRunError_RelayUnavailableSaysWhatToDo(t *testing.T) {
	err := runError(fmt.Errorf("attach: %w", program.ErrRelayUnavailable), "nats://127.0.0.1:4222")
	if !errors.Is(err, program.ErrRelayUnavailable) {
		t.Fatalf("runError lost the cause: %v", err)
	}
	for _, want := range []string{"nats://127.0.0.1:4222", "upgrade or start the dagnats server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runError = %q, want it to contain %q", err, want)
		}
	}
}

func TestRunError_Refused(t *testing.T) {
	err := runError(&program.ErrRefused{Code: program.RefusedNameTaken, Detail: "taken"}, "nats://x")
	if !strings.Contains(err.Error(), "refused to admit notify") || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("runError = %q, want the refusal", err)
	}
}

func TestRunError_OtherErrorsPassThrough(t *testing.T) {
	cause := errors.New("boom")
	if err := runError(cause, "nats://x"); err != cause {
		t.Fatalf("runError = %v, want %v unchanged", err, cause)
	}
}
