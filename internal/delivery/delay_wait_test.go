package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/engine"
)

// TestResolveDynamicDuration_ISO8601_Future proves a future RFC3339 timestamp
// at the dynamicKey path resolves to the remaining duration until that instant.
func TestResolveDynamicDuration_ISO8601_Future(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"sendAt":"2026-06-29T13:00:00Z"}`)
	got, err := resolveDynamicDuration(payload, "payload.sendAt", now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != time.Hour {
		t.Fatalf("duration = %s, want 1h", got)
	}
}

// TestResolveDynamicDuration_ISO8601_Past proves a past timestamp clamps to
// zero (no negative sleep, no error).
func TestResolveDynamicDuration_ISO8601_Past(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"sendAt":"2026-06-29T11:00:00Z"}`)
	got, err := resolveDynamicDuration(payload, "payload.sendAt", now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != 0 {
		t.Fatalf("duration = %s, want 0", got)
	}
}

// TestResolveDynamicDuration_AmountUnit proves an {amount,unit} object (nested
// via a dot-path) resolves to amount*unit, honouring the shared unit table.
func TestResolveDynamicDuration_AmountUnit(t *testing.T) {
	now := time.Now()
	payload := json.RawMessage(`{"wait":{"amount":2,"unit":"hours"}}`)
	got, err := resolveDynamicDuration(payload, "payload.wait", now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != 2*time.Hour {
		t.Fatalf("duration = %s, want 2h", got)
	}
}

// TestResolveDynamicDuration_Errors proves bad inputs (missing key, non-object
// intermediate, unparseable value, bad path root) all error.
func TestResolveDynamicDuration_Errors(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		payload string
		key     string
	}{
		{"missing key", `{"other":"x"}`, "payload.sendAt"},
		{"bad root", `{"sendAt":"2026-06-29T13:00:00Z"}`, "sendAt"},
		{"not a timestamp", `{"sendAt":"tomorrow"}`, "payload.sendAt"},
		{"zero amount", `{"wait":{"amount":0,"unit":"hours"}}`, "payload.wait"},
		{"empty key", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveDynamicDuration(json.RawMessage(tc.payload), tc.key, now); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

// TestWaitUntil_PastDurationReturnsImmediately proves a non-positive duration
// returns (nil, nil) without arming a timer.
func TestWaitUntil_PastDurationReturnsImmediately(t *testing.T) {
	out, err := waitUntil(context.Background(), 0)
	if err != nil || out != nil {
		t.Fatalf("waitUntil(0) = (%v, %v), want (nil, nil)", out, err)
	}
}

// TestWaitUntil_ContextCancel_DoesNotHang is the shutdown regression: a pending
// scheduled/dynamic delay must NOT pin its worker goroutine on a bare sleep
// (which would hang engine Stop()/wg.Wait()). With a long wait still pending,
// cancelling the context must make the handler return promptly with a
// *engine.RetryableError so dagnats requeues the wait. The 1-hour timer here
// would never elapse within the test; only the ctx-aware select returns it.
func TestWaitUntil_ContextCancel_DoesNotHang(t *testing.T) {
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
		var re *engine.RetryableError
		if !errors.As(r.err, &re) {
			t.Fatalf("err = %v (%T), want *engine.RetryableError", r.err, r.err)
		}
		if r.out != nil {
			t.Fatalf("out = %s, want nil", r.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitUntil did not return after context cancel: goroutine would pin engine WaitGroup and hang Stop()")
	}
}

// TestNavigatePayload_Nested proves the dot-path walks nested objects and
// returns the raw leaf value.
func TestNavigatePayload_Nested(t *testing.T) {
	payload := json.RawMessage(`{"a":{"b":{"c":"deep"}}}`)
	node, err := navigatePayload(payload, "payload.a.b.c")
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if string(node) != `"deep"` {
		t.Fatalf("node = %s, want \"deep\"", node)
	}
}
