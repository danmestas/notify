package emailloop

import (
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/store"
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
	id := store.NewULID()
	got, err := m.Verify(m.Mint(id))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != id {
		t.Errorf("Verify = %q, want %q", got, id)
	}
}

// TestToken_FitsALocalPart: RFC 5321 §4.5.3.1.1 caps the local part at 64
// octets and Resend enforces it (#350). The whole "reply+<token>" must fit.
func TestToken_FitsALocalPart(t *testing.T) {
	m := NewMinter(testKey())
	local := "reply+" + m.Mint(store.NewULID())
	if len(local) > 64 {
		t.Fatalf("local part is %d octets, want <= 64: %q", len(local), local)
	}
	// The token must be safe to drop into reply+<token>@domain: no '@', no '+',
	// no chars that would break a local-part.
	if strings.ContainsAny(local[6:], "@ \t\r\n+/=") {
		t.Errorf("token %q contains address-unsafe chars", local)
	}
}

func TestToken_TamperedIDRejected(t *testing.T) {
	m := NewMinter(testKey())
	tok := m.Mint(store.NewULID())
	mangled := mangleFirstChar(tok)
	if _, err := m.Verify(mangled); err == nil {
		t.Fatal("Verify accepted a tampered id")
	}
}

func TestToken_WrongKeyRejected(t *testing.T) {
	m1 := NewMinter(testKey())
	other := testKey()
	other[0] ^= 0xff
	m2 := NewMinter(other)
	if _, err := m2.Verify(m1.Mint(store.NewULID())); err == nil {
		t.Fatal("Verify accepted a token signed with a different key")
	}
}

func TestToken_MalformedRejected(t *testing.T) {
	m := NewMinter(testKey())
	for _, bad := range []string{"", ".", "abc", "abc.", ".abc", "no-dot-here", "id.notbase64!"} {
		if _, err := m.Verify(bad); err == nil {
			t.Errorf("Verify(%q) = nil error, want error", bad)
		}
	}
}

// mangleFirstChar returns s with its first byte changed to a different
// character, so the id differs but the shape still parses.
func mangleFirstChar(s string) string {
	if s == "" {
		return "A"
	}
	repl := byte('A')
	if s[0] == 'A' {
		repl = 'B'
	}
	return string(repl) + s[1:]
}
