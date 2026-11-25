package providers

import (
	"regexp"
	"testing"
)

var (
	goldenVolatile = regexp.MustCompile(`(?m)^(Date|Message-ID): .*\r\n`)
	goldenBoundary = regexp.MustCompile(`_n[0-9a-f]{32}`)
)

// normalizeMIME blanks what BuildMIME randomises, so the rest can be compared
// byte for byte.
func normalizeMIME(raw []byte) string {
	s := goldenVolatile.ReplaceAllString(string(raw), "")
	return goldenBoundary.ReplaceAllString(s, "_nBOUNDARY")
}

// TestBuildMIME_NoAttachmentsIsByteIdentical pins the exact bytes of a message
// with no attachments, so adding attachment support never changes what every
// existing email sends (#177).
func TestBuildMIME_NoAttachmentsIsByteIdentical(t *testing.T) {
	base := EmailMessage{From: "Shop <a@example.com>", To: "b@example.com", Subject: "Hello", ReplyTo: "r@example.com"}
	text, html, both := base, base, base
	text.Text = "Plain caf\u00e9 body"
	html.HTML = "<p>Hi</p>"
	both.Text, both.HTML = "Plain", "<p>Hi</p>"
	hdr := "From: Shop <a@example.com>\r\nTo: b@example.com\r\nReply-To: r@example.com\r\nSubject: Hello\r\nMIME-Version: 1.0\r\n"
	for name, c := range map[string]struct {
		msg  EmailMessage
		want string
	}{
		"text": {text, hdr + "Content-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nPlain caf=C3=A9 body"},
		"html": {html, hdr + "Content-Type: text/html; charset=\"utf-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>Hi</p>"},
		"both": {both, hdr + "Content-Type: multipart/alternative; boundary=\"_nBOUNDARY\"\r\n\r\n" +
			"--_nBOUNDARY\r\nContent-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nPlain\r\n" +
			"--_nBOUNDARY\r\nContent-Type: text/html; charset=\"utf-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>Hi</p>\r\n" +
			"--_nBOUNDARY--\r\n"},
	} {
		raw, _ := BuildMIME(c.msg)
		if got := normalizeMIME(raw); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}
