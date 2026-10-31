package aibuilder

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestAgentLoop_StateOverCapFailsClearly proves a conversation that outgrows
// the engine's agent-loop state cap ends the build with an error naming the
// cap and its setting, both returned and streamed, not an opaque failure.
func TestAgentLoop_StateOverCapFailsClearly(t *testing.T) {
	capErr := errors.New("Continue: state is 600000 bytes, over the 524288 byte cap (loop_state_max_bytes)")
	lc := &fakeLoopCtx{continueErr: capErr}
	b := &AgentLoopBuilder{}

	err := b.continueLoop(lc, &loopState{SessionID: "s1"})
	if !errors.Is(err, capErr) {
		t.Fatalf("err = %v, want it to wrap the engine's error", err)
	}
	if !strings.Contains(err.Error(), "DAGNATS_LOOP_STATE_MAX_BYTES") {
		t.Fatalf("err = %q, want it to name DAGNATS_LOOP_STATE_MAX_BYTES", err)
	}
	if len(lc.streams) != 1 {
		t.Fatalf("streamed %d events, want 1 KindError", len(lc.streams))
	}
	var ev streamEvent
	if err := json.Unmarshal(lc.streams[0], &ev); err != nil {
		t.Fatalf("decode stream event: %v", err)
	}
	if ev.Kind != KindError || !strings.Contains(ev.Error, "DAGNATS_LOOP_STATE_MAX_BYTES") {
		t.Fatalf("stream event = %+v, want a KindError naming the setting", ev)
	}
}
