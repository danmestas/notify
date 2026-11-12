package smsloop

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
)

// hexSig is SignalWire's signature: hex(HMAC-SHA1(key, url + raw body)).
func hexSig(key, fullURL, raw string) string {
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(fullURL + raw))
	return hex.EncodeToString(mac.Sum(nil))
}
