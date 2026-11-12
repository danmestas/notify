package signalwire

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"testing"

	"github.com/danmestas/notify/internal/providers/twilio"
)

// TestValidSignature: SignalWire signs with the project's Signing Key, as
// hex(HMAC-SHA1(key, URL + raw body)); its Compatibility API may instead send
// the Twilio form over the same key. Both are accepted, nothing else is.
func TestValidSignature(t *testing.T) {
	const key = "PSKsigningkey"
	const full = "https://notify.example.com/v1/inbound/sms/int-1"
	raw := []byte("MessageSid=SM1&From=%2B15551112222&To=%2B15559990000&Body=hello")

	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(full))
	mac.Write(raw)
	hexSig := hex.EncodeToString(mac.Sum(nil))

	params, err := parseForm(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	compat := twilio.Signature(key, full, params)

	for name, sig := range map[string]string{"hex over the raw body": hexSig, "twilio-compatible": compat} {
		if !ValidSignature(key, full, raw, sig) {
			t.Errorf("%s: refused", name)
		}
	}
	for name, c := range map[string]struct {
		key, url string
		raw      []byte
		sig      string
	}{
		"empty signature": {key, full, raw, ""},
		"wrong key":       {"other", full, raw, hexSig},
		"another url":     {key, full + "/status", raw, hexSig},
		"tampered body":   {key, full, []byte("MessageSid=SM1&Body=STOP"), hexSig},
		"empty key":       {"", full, raw, hexSig},
	} {
		if ValidSignature(c.key, c.url, c.raw, c.sig) {
			t.Errorf("%s: accepted", name)
		}
	}
}
