package secretbox_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/secretbox"
)

func newBox(t *testing.T) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// TestRoundTrip is the core contract: what goes in comes back out.
func TestRoundTrip(t *testing.T) {
	b := newBox(t)
	for _, plain := range []string{
		"Bearer sk-live-abc123",
		`{"Authorization":"Bearer x","X-Api-Key":"y"}`,
		"unicode: café ☕",
	} {
		sealed, err := b.Seal(plain)
		if err != nil {
			t.Fatalf("Seal(%q): %v", plain, err)
		}
		got, err := b.Open(sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got != plain {
			t.Errorf("round trip: got %q, want %q", got, plain)
		}
	}
}

// TestSealedIsNotPlaintext is the property the whole feature rests on: the
// stored form must not contain the secret.
func TestSealedIsNotPlaintext(t *testing.T) {
	b := newBox(t)
	const secret = "Bearer sk-live-super-secret"

	sealed, err := b.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "sk-live-super-secret") {
		t.Fatalf("sealed value contains the plaintext secret: %q", sealed)
	}
	if !secretbox.IsSealed(sealed) {
		t.Fatalf("Seal output is not recognised as sealed: %q", sealed)
	}
}

// TestOpenPassesThroughLegacyPlaintext keeps rows written before encryption
// existed readable: an unmarked value is returned unchanged, not treated as
// corrupt ciphertext.
func TestOpenPassesThroughLegacyPlaintext(t *testing.T) {
	b := newBox(t)
	const legacy = `{"Authorization":"Bearer old-plaintext"}`

	if secretbox.IsSealed(legacy) {
		t.Fatal("legacy plaintext should not be recognised as sealed")
	}
	got, err := b.Open(legacy)
	if err != nil {
		t.Fatalf("Open(legacy): %v", err)
	}
	if got != legacy {
		t.Errorf("Open(legacy) = %q, want it unchanged", got)
	}
}

// TestEmptySealsToEmpty keeps an absent secret absent, rather than turning it
// into a ciphertext blob that reads as a set value.
func TestEmptySealsToEmpty(t *testing.T) {
	b := newBox(t)
	sealed, err := b.Seal("")
	if err != nil {
		t.Fatalf("Seal(\"\"): %v", err)
	}
	if sealed != "" {
		t.Errorf("Seal(\"\") = %q, want empty", sealed)
	}
}

// TestSealIsNonDeterministic proves the nonce is random: the same plaintext
// seals to different tokens, so ciphertext does not leak equality of secrets.
func TestSealIsNonDeterministic(t *testing.T) {
	b := newBox(t)
	a, _ := b.Seal("same-secret")
	c, _ := b.Seal("same-secret")
	if a == c {
		t.Fatal("two seals of the same plaintext are identical: nonce is not random")
	}
}

// TestOpenRejectsTamper proves the value is authenticated: a flipped byte fails
// rather than returning altered plaintext.
func TestOpenRejectsTamper(t *testing.T) {
	b := newBox(t)
	sealed, _ := b.Seal("Bearer x")

	// Flip a byte in the base64 payload.
	tampered := []byte(sealed)
	tampered[len(tampered)-2] ^= 0x01
	if _, err := b.Open(string(tampered)); err == nil {
		t.Fatal("Open accepted a tampered ciphertext")
	}
}

// TestWrongKeyCannotOpen proves the key is load-bearing.
func TestWrongKeyCannotOpen(t *testing.T) {
	b := newBox(t)
	sealed, _ := b.Seal("Bearer x")

	other, err := secretbox.New(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatalf("New(other): %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a different key opened the ciphertext")
	}
}
