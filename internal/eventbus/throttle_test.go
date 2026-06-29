package eventbus

import (
	"context"
	"encoding/base64"
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

// TestThrottleKey_InjectiveAfterSanitize proves that the base64url-component key
// format used by delivery.ThrottleHandler is injective after sanitizeKey. Two
// tuples that differ only in where an underscore-vs-separator falls must
// produce DIFFERENT bucket keys.
//
// Historical note: the OLD colon-join key ("env:sub:wf") was NOT injective:
// sanitizeKey maps ':' to '_', so "e:user_123:abc" and "e:user:123_abc" both
// became "e_user_123_abc" — sharing a counter across different subscribers and
// workflows. The new base64url format (b64(env) + "." + b64(sub) + "." +
// b64(wf)) is injective because base64url's alphabet (A-Za-z0-9-_) survives
// sanitizeKey unchanged, and the dot separator is never emitted by base64url.
func TestThrottleKey_InjectiveAfterSanitize(t *testing.T) {
	b64 := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	newKey := func(env, sub, wf string) string {
		return b64(env) + "." + b64(sub) + "." + b64(wf)
	}

	// These two tuples collide under the old colon-join scheme:
	//   "e:user_123:abc" → sanitize → "e_user_123_abc"
	//   "e:user:123_abc" → sanitize → "e_user_123_abc"  ← same!
	//
	// Verify the old scheme does in fact collide (documents the bug):
	if sanitizeKey("e:user_123:abc") != sanitizeKey("e:user:123_abc") {
		t.Errorf("old collision assumption wrong — re-examine test setup")
	}

	// Under the new scheme the same two logical tuples must not collide.
	k1 := sanitizeKey(newKey("e", "user_123", "abc"))
	k2 := sanitizeKey(newKey("e", "user", "123_abc"))
	if k1 == k2 {
		t.Errorf("key collision after sanitizeKey: %q == %q (tuples differ only in where '_' falls)", k1, k2)
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
