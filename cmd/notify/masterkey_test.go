package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestMasterKey pins masterKey's production-guard behavior (notify hardening
// #2): an empty hex key is only ever accepted in dev mode, where it must keep
// deriving the exact same fixed key as before so existing local users are not
// re-keyed. Outside dev, an empty hex key is a fatal misconfiguration.
func TestMasterKey(t *testing.T) {
	fixedDevKey := sha256.Sum256([]byte("notify-dev-master-key"))
	validHex := strings.Repeat("ab", 32) // decodes to exactly 32 bytes
	validKey, err := hex.DecodeString(validHex)
	if err != nil {
		t.Fatalf("fixture hex: %v", err)
	}

	cases := []struct {
		name    string
		hexKey  string
		dev     bool
		wantErr bool
		wantKey []byte
	}{
		{name: "empty hex, not dev: fatal error, no key derived", hexKey: "", dev: false, wantErr: true},
		{name: "empty hex, dev: derives the fixed dev key byte-exact", hexKey: "", dev: true, wantKey: fixedDevKey[:]},
		{name: "valid hex, not dev: decodes", hexKey: validHex, dev: false, wantKey: validKey},
		{name: "valid hex, dev=true: explicit key wins over dev", hexKey: validHex, dev: true, wantKey: validKey},
		{name: "invalid hex characters: error", hexKey: "not-hex-zz", dev: false, wantErr: true},
		{name: "wrong-length hex (16 bytes): error", hexKey: strings.Repeat("ab", 16), dev: false, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := masterKey(tc.hexKey, tc.dev)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("masterKey(%q, %v) = nil error, want error", tc.hexKey, tc.dev)
				}
				return
			}
			if err != nil {
				t.Fatalf("masterKey(%q, %v) unexpected error: %v", tc.hexKey, tc.dev, err)
			}
			if !bytes.Equal(got, tc.wantKey) {
				t.Errorf("masterKey(%q, %v) = %x, want %x", tc.hexKey, tc.dev, got, tc.wantKey)
			}
		})
	}
}
