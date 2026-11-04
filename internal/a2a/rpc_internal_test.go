package a2a

import (
	"strings"
	"testing"
)

// A caller must never choose the client_message_id other parts of notify
// match on (review-mode drafts key on it per environment).
func TestIdempotencyKeyIsNeverTheCallersText(t *testing.T) {
	draftKey := strings.Repeat("ab", 32) // a 64-hex workflow draft key
	k := idempotencyKey("cred1", draftKey)
	if k == draftKey || !strings.HasPrefix(k, "a") || len(k) != 41 {
		t.Fatalf("key = %q", k)
	}
	first, retry := idempotencyKey("cred1", "m1"), idempotencyKey("cred1", "m1")
	if first != retry {
		t.Fatal("a retry must map to the same key")
	}
	if idempotencyKey("cred1", "m1") == idempotencyKey("cred2", "m1") {
		t.Fatal("two credentials must not share a key for the same messageId")
	}
}
