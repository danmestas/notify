package signalwire

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"net/url"
	"strings"

	"github.com/danmestas/notify/internal/providers/twilio"
)

// SignatureHeader carries SignalWire's request signature on a webhook POST.
// Its Compatibility API may send the same value as X-Twilio-Signature.
const SignatureHeader = "X-SignalWire-Signature"

// ValidSignature reports whether sig signs a POST of rawBody to fullURL under
// the project's Signing Key. SignalWire's scheme is hex(HMAC-SHA1(key, fullURL
// + rawBody)); its Compatibility API also signs the Twilio way
// (base64(HMAC-SHA1(key, fullURL + sorted form params))). Either is accepted:
// both are keyed by the same secret. An empty key or signature is never valid.
func ValidSignature(signingKey, fullURL string, rawBody []byte, sig string) bool {
	if signingKey == "" || sig == "" {
		return false
	}
	mac := hmac.New(sha1.New, []byte(signingKey))
	mac.Write([]byte(fullURL))
	mac.Write(rawBody)
	if hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(strings.ToLower(sig))) {
		return true
	}
	params, err := parseForm(rawBody)
	if err != nil {
		return false
	}
	return twilio.ValidSignature(signingKey, fullURL, params, sig)
}

func parseForm(raw []byte) (url.Values, error) {
	return url.ParseQuery(string(raw))
}
