package engine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/engine"
)

// TestStartRefused separates a start that certainly left no run from one whose
// outcome is unknown: only the latter may have started a run whose reply was
// lost (#66).
func TestStartRefused(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dagnats error reply", fmt.Errorf("%w: engine: workflow not found", engine.ErrStartRefused), true},
		{"no responders", fmt.Errorf("nats request: %w", nats.ErrNoResponders), true},
		{"timeout", fmt.Errorf("nats request: %w", nats.ErrTimeout), false},
		{"deadline", fmt.Errorf("nats request: %w", context.DeadlineExceeded), false},
		{"cancelled", fmt.Errorf("nats request: %w", context.Canceled), false},
		{"connection closed", fmt.Errorf("nats request: %w", nats.ErrConnectionClosed), false},
		{"other", errors.New("decode start-run reply: unexpected EOF"), false},
	}
	for _, tc := range cases {
		if got := engine.StartRefused(tc.err); got != tc.want {
			t.Errorf("%s: StartRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}
