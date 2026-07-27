// Package secretbox provides marked, AES-256-GCM authenticated encryption for
// short secrets stored as text (e.g. request-header values in a step's control
// values).
//
// Two properties make it safe to apply to a field that may already hold legacy
// plaintext:
//
//   - Sealed values carry a version marker prefix, so Open can tell ciphertext
//     from a value written before encryption existed and pass plaintext through
//     unchanged. Migrations are unnecessary; old rows keep working.
//   - IsSealed lets a writer skip re-encrypting an already-sealed value, so
//     re-saving a step does not double-encrypt.
//
// The construction mirrors the env-var store's (AES-256-GCM, random nonce
// prepended to the ciphertext); this is a separate, self-contained helper so
// adopting it here does not disturb that working code.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// marker prefixes every sealed value. The "1" is a format version: if the
// scheme ever changes, Open can dispatch on it while still reading v1 rows.
const marker = "enc:1:"

// Box seals and opens secrets with a fixed master key.
type Box struct {
	gcm cipher.AEAD
}

// New builds a Box from a 32-byte AES-256 master key.
func New(masterKey []byte) (*Box, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new gcm: %w", err)
	}
	return &Box{gcm: gcm}, nil
}

// IsSealed reports whether s was produced by Seal. Writers use it to avoid
// re-sealing an already-encrypted value.
func IsSealed(s string) bool {
	return strings.HasPrefix(s, marker)
}

// Seal encrypts plaintext and returns a marked, base64 token. An empty string
// seals to empty: an absent secret stays absent rather than becoming a
// ciphertext blob that looks like a set value.
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	sealed := b.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return marker + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal. A value without the marker is treated as legacy plaintext
// and returned unchanged, so rows written before encryption keep working.
func (b *Box) Open(s string) (string, error) {
	if !IsSealed(s) {
		return s, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, marker))
	if err != nil {
		return "", fmt.Errorf("secretbox: decode: %w", err)
	}
	ns := b.gcm.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("secretbox: ciphertext shorter than nonce")
	}
	plain, err := b.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("secretbox: open: %w", err)
	}
	return string(plain), nil
}
