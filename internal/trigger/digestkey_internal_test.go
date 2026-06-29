package trigger

import (
	"encoding/json"
	"testing"
)

func TestDigestKeyPath(t *testing.T) {
	cases := []struct {
		cv   string
		want string
	}{
		{`{"type":"regular","digestKey":"payload.orderId"}`, "payload.orderId"},
		{`{"type":"regular","amount":1,"unit":"seconds"}`, ""},
		{``, ""},
		{`{"digestKey":""}`, ""},
	}
	for _, c := range cases {
		if got := digestKeyPath(json.RawMessage(c.cv)); got != c.want {
			t.Errorf("digestKeyPath(%s) = %q, want %q", c.cv, got, c.want)
		}
	}
}

func TestResolveDigestKeyValue(t *testing.T) {
	payload := json.RawMessage(`{"orderId":"A-1","count":3,"nested":{"plan":"pro"}}`)
	subscriber := json.RawMessage(`{"subscriberId":"u1","data":{"plan":"premium"}}`)
	cases := []struct {
		path string
		want string
	}{
		{"payload.orderId", "A-1"},
		{"payload.count", "3"},
		{"payload.nested.plan", "pro"},
		{"subscriber.data.plan", "premium"},
		{"subscriber.subscriberId", "u1"},
		{"payload.missing", ""},
		{"payload.nested.absent", ""},
		{"unknownRoot.x", ""},
		{"payload", ""}, // a single segment is not a valid dot-path
		{"", ""},
	}
	for _, c := range cases {
		if got := resolveDigestKeyValue(c.path, payload, subscriber); got != c.want {
			t.Errorf("resolveDigestKeyValue(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestBuildDigestKey_EmptyValueFallsBackToBase proves that when the digestKey
// dot-path resolves to the empty string (field absent) the key is the bare
// envID:subID:wfID base with NO trailing segment — identical to the no-digestKey
// baseline, so absent-field events merge into the per-subscriber bucket.
func TestBuildDigestKey_EmptyValueFallsBackToBase(t *testing.T) {
	base := "env:sub:wf"
	if got := buildDigestKey(base, ""); got != base {
		t.Fatalf("buildDigestKey(empty) = %q, want %q", got, base)
	}
}

// TestBuildDigestKey_InjectiveEncoding proves two resolved values that differ
// only by a character the NATS-KV sanitizer collapses (':' vs '_' both become
// '_') still produce DISTINCT digest keys — the value segment is encoded so
// "order:001" and "order_001" can never silently merge into one bucket.
func TestBuildDigestKey_InjectiveEncoding(t *testing.T) {
	base := "env:sub:wf"
	colon := buildDigestKey(base, "order:001")
	under := buildDigestKey(base, "order_001")
	if colon == under {
		t.Fatalf("buildDigestKey collided: %q == %q (values must stay distinct)", colon, under)
	}
	if colon == base || under == base {
		t.Fatalf("non-empty value must extend the base key, got %q / %q", colon, under)
	}
}
