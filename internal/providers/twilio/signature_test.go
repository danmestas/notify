package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"testing"
)

// TestSignature_PublishedVector is the worked example from Twilio's
// "Webhooks security" page (twilio.com/docs/usage/security).
func TestSignature_PublishedVector(t *testing.T) {
	params := url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Caller":  {"+14158675310"},
		"Digits":  {"1234"},
		"From":    {"+14158675310"},
		"To":      {"+18005551212"},
	}
	const want = "L/OH5YylLD5NRKLltdqwSvS0BnU="
	if got := Signature("12345", "https://example.com/myapp.php?foo=1&bar=2", params); got != want {
		t.Fatalf("Signature = %q, want Twilio's %q", got, want)
	}
}

// TestValidSignature: X-Twilio-Signature is base64(HMAC-SHA1(auth token,
// URL + each POST param's name and value, names sorted)).
func TestValidSignature(t *testing.T) {
	const token = "auth-token"
	const full = "https://notify.example.com/v1/inbound/sms/int-1"
	params := url.Values{"To": {"+15559990000"}, "Body": {"hi there"}, "From": {"+15551112222"}, "MessageSid": {"SM1"}}

	// Sorted by name: Body, From, MessageSid, To.
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(full + "Body" + "hi there" + "From" + "+15551112222" + "MessageSid" + "SM1" + "To" + "+15559990000"))
	good := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got := Signature(token, full, params); got != good {
		t.Fatalf("Signature = %q, want %q", got, good)
	}
	if !ValidSignature(token, full, params, good) {
		t.Fatal("the correct signature was refused")
	}
	tampered := url.Values{}
	for k, v := range params {
		tampered[k] = v
	}
	tampered.Set("Body", "STOP")
	for name, c := range map[string]struct {
		token, url string
		params     url.Values
		sig        string
	}{
		"empty signature":  {token, full, params, ""},
		"wrong token":      {"other", full, params, good},
		"another url":      {token, full + "/status", params, good},
		"tampered body":    {token, full, tampered, good},
		"garbage":          {token, full, params, "not-base64"},
		"empty auth token": {"", full, params, Signature("", full, params)},
	} {
		if ValidSignature(c.token, c.url, c.params, c.sig) {
			t.Errorf("%s: accepted", name)
		}
	}
}
