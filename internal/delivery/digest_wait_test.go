package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/engine"
)

// TestDigestWait_ContextCancel_RetryableError is the shutdown-safety regression
// for the digest.wait handler: HandleDigestWait delegates its wait to
// waitUntil, which returns a *engine.RetryableError on ctx cancellation so
// dagnats requeues the task instead of dead-lettering it. Without the requeue,
// a process restart during a digest cron window permanently drops the digest.
//
// This test exercises waitUntil directly — the same function HandleDigestWait
// uses for its wait phase after resolving the cron duration — mirroring
// TestWaitUntil_ContextCancel_DoesNotHang from delay_wait_test.go.
func TestDigestWait_ContextCancel_RetryableError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		out json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := waitUntil(ctx, time.Hour)
		done <- result{out, err}
	}()
	cancel()
	select {
	case r := <-done:
		if r.out != nil {
			t.Fatalf("waitUntil returned non-nil output on cancel: %s", r.out)
		}
		var re *engine.RetryableError
		if !errors.As(r.err, &re) {
			t.Fatalf("waitUntil on cancel: want *engine.RetryableError, got %T: %v", r.err, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitUntil did not return after context cancel (goroutine pinned — digest would be dropped on restart)")
	}
}
