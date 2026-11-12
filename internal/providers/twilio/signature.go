package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// SignatureHeader carries Twilio's request signature on a webhook POST.
const SignatureHeader = "X-Twilio-Signature"

// Signature returns the X-Twilio-Signature Twilio sends for a POST of params
// to fullURL: base64(HMAC-SHA1(authToken, fullURL followed by every param's
// name and value, names sorted, a repeated name's values in order)). fullURL
// is exactly the URL configured at Twilio, query string included.
func Signature(authToken, fullURL string, params url.Values) string {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, name := range names {
		for _, v := range params[name] {
			b.WriteString(name)
			b.WriteString(v)
		}
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ValidSignature reports whether sig is the signature of a POST of params to
// fullURL under authToken. An empty token or signature is never valid.
func ValidSignature(authToken, fullURL string, params url.Values, sig string) bool {
	if authToken == "" || sig == "" {
		return false
	}
	return hmac.Equal([]byte(Signature(authToken, fullURL, params)), []byte(sig))
}
