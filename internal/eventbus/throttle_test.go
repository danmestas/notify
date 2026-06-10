package eventbus

import (
	"context"
	"testing"
	"time"
)

func TestBus_ThrottleAllow_UnderLimit(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	const key = "user-42"
	for i := 1; i <= 3; i++ {
		allowed, err := bus.ThrottleAllow(key, 3, 10*time.Second)
		if err != nil {
			t.Fatalf("ThrottleAllow #%d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("ThrottleAllow #%d: got allowed=false, want true", i)
		}
	}

	allowed, err := bus.ThrottleAllow(key, 3, 10*time.Second)
	if err != nil {
		t.Fatalf("ThrottleAllow #4: %v", err)
	}
	if allowed {
		t.Fatalf("ThrottleAllow #4: got allowed=true, want false (over limit)")
	}
}

func TestBus_ThrottleAllow_SeparateKeys(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	// Exhaust key "a".
	if allowed, err := bus.ThrottleAllow("a", 1, 10*time.Second); err != nil || !allowed {
		t.Fatalf("ThrottleAllow(a) first: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := bus.ThrottleAllow("a", 1, 10*time.Second); err != nil || allowed {
		t.Fatalf("ThrottleAllow(a) second: allowed=%v err=%v, want allowed=false", allowed, err)
	}

	// Key "b" must be unaffected.
	if allowed, err := bus.ThrottleAllow("b", 1, 10*time.Second); err != nil || !allowed {
		t.Fatalf("ThrottleAllow(b): allowed=%v err=%v, want allowed=true", allowed, err)
	}
}

func TestBus_ThrottleAllow_WindowReset(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	const (
		key    = "reset-key"
		window = 1 * time.Second
	)

	if allowed, err := bus.ThrottleAllow(key, 1, window); err != nil || !allowed {
		t.Fatalf("ThrottleAllow first: allowed=%v err=%v, want true", allowed, err)
	}
	if allowed, err := bus.ThrottleAllow(key, 1, window); err != nil || allowed {
		t.Fatalf("ThrottleAllow second: allowed=%v err=%v, want false", allowed, err)
	}

	// Sleep just past the window so the next call lands in a fresh bucket.
	time.Sleep(window + 200*time.Millisecond)

	if allowed, err := bus.ThrottleAllow(key, 1, window); err != nil || !allowed {
		t.Fatalf("ThrottleAllow after window: allowed=%v err=%v, want true", allowed, err)
	}
}
