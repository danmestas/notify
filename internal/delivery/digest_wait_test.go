package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/engine"
)

// TestDigestWait_ResumedTickFires proves a digest parked until its cron tick
// fires when it resumes at that tick, instead of recomputing "the next tick
// after now", which would be a full period later and loop forever. The run has
// no stored config here on purpose: a resume must not need to recompute.
func TestDigestWait_ResumedTickFires(t *testing.T) {
	d := NewDigestWait(newStore(t))
	out, err := d.HandleDigestWait(context.Background(), engine.TaskPayload{
		RunID: "run_digest", StepID: "S1", ResumeAt: time.Now().Add(-time.Millisecond),
	})
	if err != nil || out != nil {
		t.Fatalf("resumed digest = (%s, %v), want it to fire with (nil, nil)", out, err)
	}
}

// TestDigestWait_EarlyResumeParksAgain proves a digest redelivered before its
// tick (a restart, or a park chunk shorter than the wait) parks again toward
// the same tick rather than firing early or moving the tick.
func TestDigestWait_EarlyResumeParksAgain(t *testing.T) {
	d := NewDigestWait(newStore(t))
	tick := time.Now().Add(time.Hour)
	_, err := d.HandleDigestWait(context.Background(), engine.TaskPayload{RunID: "run_digest", StepID: "S1", ResumeAt: tick})
	assertParkedAt(t, err, tick)
}

func assertParkedAt(t *testing.T, err error, at time.Time) {
	t.Helper()
	we, ok := err.(*engine.WaitError)
	if !ok || !we.At.Equal(at) {
		t.Fatalf("err = %v (%T), want a park until %v", err, err, at)
	}
}
