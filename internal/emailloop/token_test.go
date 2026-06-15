package emailloop

import (
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestToken_MintVerifyRoundTrip(t *testing.T) {
	m := NewMinter(testKey())
	scope := domain.Scope{OrgID: "org-a", EnvID: "env-a"}
	tok := m.Mint(scope, "thread-1", "sub-9")

	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.OrgID != "org-a" || claims.EnvID != "env-a" {
		t.Errorf("scope = %+v, want org-a/env-a", claims)
	}
	if claims.ThreadID != "thread-1" {
		t.Errorf("ThreadID = %q, want thread-1", claims.ThreadID)
	}
	if claims.SubscriberID != "sub-9" {
		t.Errorf("SubscriberID = %q, want sub-9", claims.SubscriberID)
	}
}

func TestToken_AddressSafe(t *testing.T) {
	m := NewMinter(testKey())
	tok := m.Mint(domain.Scope{OrgID: "o", EnvID: "e"}, "t", "s")
	// The token must be safe to drop into reply+<token>@domain: no '@', no '+',
	// no chars that would break a local-part.
	if strings.ContainsAny(tok, "@ \t\r\n+/=") {
		t.Errorf("token %q contains address-unsafe chars", tok)
	}
}

func TestToken_TamperedPayloadRejected(t *testing.T) {
	m := NewMinter(testKey())
	tok := m.Mint(domain.Scope{OrgID: "o", EnvID: "e"}, "t", "s")
	// Flip a character in the payload segment.
	parts := strings.SplitN(tok, ".", 2)
	mangled := mangleFirstChar(parts[0]) + "." + parts[1]
	if _, err := m.Verify(mangled); err == nil {
		t.Fatal("Verify accepted a tampered payload")
	}
}

func TestToken_WrongKeyRejected(t *testing.T) {
	m1 := NewMinter(testKey())
	other := testKey()
	other[0] ^= 0xff
	m2 := NewMinter(other)

	tok := m1.Mint(domain.Scope{OrgID: "o", EnvID: "e"}, "t", "s")
	if _, err := m2.Verify(tok); err == nil {
		t.Fatal("Verify accepted a token signed with a different key")
	}
}

func TestToken_MalformedRejected(t *testing.T) {
	m := NewMinter(testKey())
	for _, bad := range []string{"", ".", "abc", "abc.", ".abc", "no-dot-here"} {
		if _, err := m.Verify(bad); err == nil {
			t.Errorf("Verify(%q) = nil error, want error", bad)
		}
	}
}

// mangleFirstChar returns s with its first byte changed to a different valid
// base64url char, so the decoded payload differs but still decodes.
func mangleFirstChar(s string) string {
	if s == "" {
		return "A"
	}
	c := s[0]
	repl := byte('A')
	if c == 'A' {
		repl = 'B'
	}
	return string(repl) + s[1:]
}
