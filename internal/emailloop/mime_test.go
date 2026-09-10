package emailloop

import (
	"strings"
	"testing"
)

// crlf builds a wire-format message from LF source, the way every fixture
// below is written for readability.
func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(strings.TrimLeft(s, "\n"), "\n", "\r\n"))
}

func TestParseMessage_PlainText(t *testing.T) {
	raw := crlf(`
From: Dev <dev@example.com>
To: reply+tok123@mail.example.org
Subject: Re: Build failed
Message-ID: <abc@example.com>
In-Reply-To: <orig@mail.example.org>
References: <first@mail.example.org> <orig@mail.example.org>
Content-Type: text/plain; charset=utf-8

Looks like a flake, rerunning.

On Tue, Sep 9, 2026, Quarry wrote:
> Build failed on main
`)
	got, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.From != "Dev <dev@example.com>" || got.To != "reply+tok123@mail.example.org" {
		t.Errorf("from=%q to=%q", got.From, got.To)
	}
	if got.Subject != "Re: Build failed" {
		t.Errorf("subject=%q", got.Subject)
	}
	if got.InReplyTo != "<orig@mail.example.org>" || len(got.References) != 2 {
		t.Errorf("threading: inReplyTo=%q references=%v", got.InReplyTo, got.References)
	}
	if !strings.HasPrefix(got.Text, "Looks like a flake, rerunning.\n") {
		t.Errorf("text=%q", got.Text)
	}
	if strings.Contains(got.Text, "\r") {
		t.Errorf("text kept CR line endings, which defeats the quoted-history markers")
	}
	// The whole point: the same recipient extraction the webhook uses.
	if tok, host, ok := extractToken(got.recipient()); !ok || tok != "tok123" || host != "mail.example.org" {
		t.Errorf("recipient token = %q host=%q ok=%v", tok, host, ok)
	}
	// And bestBody strips the quoted history exactly as it does for webhook input.
	if body := bestBody(got); body != "Looks like a flake, rerunning." {
		t.Errorf("bestBody = %q", body)
	}
}

// TestParseMessage_MultipartAlternative is the shape every real mail client
// sends: text and HTML siblings. Text must win for Text, HTML for HTML, and
// each is decoded from its own transfer encoding.
func TestParseMessage_MultipartAlternative(t *testing.T) {
	raw := crlf(`
From: dev@example.com
To: reply+tok@mail.example.org
Subject: =?UTF-8?Q?Re=3A_caf=C3=A9_build?=
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="b1"

--b1
Content-Type: text/plain; charset="utf-8"
Content-Transfer-Encoding: quoted-printable

caf=C3=A9 is back up
--b1
Content-Type: text/html; charset="utf-8"
Content-Transfer-Encoding: base64

PHA+Y2Fmw6kgaXMgYmFjayB1cDwvcD4=
--b1--
`)
	got, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.Subject != "Re: café build" {
		t.Errorf("RFC 2047 subject not decoded: %q", got.Subject)
	}
	if got.Text != "café is back up" {
		t.Errorf("quoted-printable text not decoded: %q", got.Text)
	}
	if got.HTML != "<p>café is back up</p>" {
		t.Errorf("base64 html not decoded: %q", got.HTML)
	}
	if len(got.Attachments) != 0 {
		t.Errorf("alternative parts were filed as attachments: %+v", got.Attachments)
	}
}

// TestParseMessage_Latin1 covers a charset that is not UTF-8: the bytes must
// be transcoded, not passed through as mojibake.
func TestParseMessage_Latin1(t *testing.T) {
	raw := append(crlf(`
From: dev@example.com
To: reply+tok@mail.example.org
Subject: hi
Content-Type: text/plain; charset="iso-8859-1"

`), []byte("caf\xe9\r\n")...)
	got, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.Text != "café\n" {
		t.Errorf("latin1 not transcoded: %q", got.Text)
	}
}

// TestParseMessage_MixedWithAttachment: a reply with a file attached still
// threads its text, and the attachment is recorded by name/type/size without
// its bytes being retained.
func TestParseMessage_MixedWithAttachment(t *testing.T) {
	raw := crlf(`
From: dev@example.com
To: reply+tok@mail.example.org
Subject: log attached
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain

see attached
--inner
Content-Type: text/html

<b>see attached</b>
--inner--
--outer
Content-Type: application/octet-stream; name="build.log"
Content-Disposition: attachment; filename="build.log"
Content-Transfer-Encoding: base64

aGVsbG8gbG9nCg==
--outer--
`)
	got, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.Text != "see attached" || got.HTML != "<b>see attached</b>" {
		t.Errorf("nested alternative not walked: text=%q html=%q", got.Text, got.HTML)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want one", got.Attachments)
	}
	a := got.Attachments[0]
	if a.Filename != "build.log" || a.ContentType != "application/octet-stream" || a.Size != len("hello log\n") {
		t.Errorf("attachment = %+v", a)
	}
}

// TestParseMessage_DeliveredToIsEnvelope: an MTA piping to a command adds
// Delivered-To for the envelope recipient, which is more reliable than a To
// header a forwarder may have rewritten.
func TestParseMessage_DeliveredToIsEnvelope(t *testing.T) {
	raw := crlf(`
Delivered-To: reply+realtok@mail.example.org
From: dev@example.com
To: Some List <list@example.com>
Subject: hi

body
`)
	got, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.EnvelopeTo != "reply+realtok@mail.example.org" {
		t.Errorf("EnvelopeTo = %q", got.EnvelopeTo)
	}
	if tok, _, ok := extractToken(got.recipient()); !ok || tok != "realtok" {
		t.Errorf("recipient() did not prefer the envelope: %q", got.recipient())
	}
}

// TestParseMessage_Limits: oversize is refused before parsing, and a
// malformed container keeps what it already found rather than losing the
// reply.
func TestParseMessage_Limits(t *testing.T) {
	if _, err := ParseMessage(make([]byte, MaxMessageBytes+1)); err != ErrMessageTooLarge {
		t.Errorf("oversize: err = %v, want ErrMessageTooLarge", err)
	}
	if _, err := ParseMessage([]byte("not a message at all")); err == nil {
		t.Error("garbage parsed without error")
	}
	// Truncated multipart: the text part before the cut still threads.
	truncated := crlf(`
From: a@b
To: reply+t@d
Subject: s
Content-Type: multipart/alternative; boundary="b1"

--b1
Content-Type: text/plain

partial but present
--b1
Content-Type: text/html
`)
	got, err := ParseMessage(truncated)
	if err != nil {
		t.Fatalf("truncated multipart: %v", err)
	}
	if got.Text != "partial but present" {
		t.Errorf("text lost from a truncated container: %q", got.Text)
	}
}

// TestParseMessage_FeedsAccept ties the parser to the shared path: a raw
// message for an armed mailbox threads through Accept exactly like the
// webhook payload would, with the same status.
func TestParseMessage_FeedsAccept(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := t.Context()
	scope, threadID, _ := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "dev@example.com")
	token := f.token(t, scope, threadID)

	raw := crlf(`
From: dev@example.com
To: reply+` + token + `@mail.example.org
Subject: Re: hi
Content-Type: text/plain

on it
`)
	parsed, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	status, err := f.svc.Accept(ctx, parsed)
	if err != nil || status != StatusThreaded {
		t.Fatalf("Accept = %q, %v; want threaded", status, err)
	}
	if f.dispatchCount() != 1 || f.dispatches[0].Text != "on it" {
		t.Fatalf("responder dispatches = %+v", f.dispatches)
	}
}

// TestParseMessage_TopLevelTransferEncoding: mime/multipart decodes
// quoted-printable parts on its own, but net/mail leaves a single-part body
// exactly as sent, so a non-multipart reply -- what a minimal client or a
// script sends -- depends on the parser's own decoding.
func TestParseMessage_TopLevelTransferEncoding(t *testing.T) {
	for _, tc := range []struct{ name, cte, body, want string }{
		{"quoted-printable", "quoted-printable", "caf=C3=A9 is back up", "café is back up"},
		{"base64", "base64", "Y2Fmw6kgaXMgYmFjayB1cA==", "café is back up"},
		{"7bit", "7bit", "plain", "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := crlf(`
From: dev@example.com
To: reply+tok@mail.example.org
Subject: hi
Content-Type: text/plain; charset="utf-8"
Content-Transfer-Encoding: ` + tc.cte + `

` + tc.body + `
`)
			got, err := ParseMessage(raw)
			if err != nil {
				t.Fatalf("ParseMessage: %v", err)
			}
			if strings.TrimSpace(got.Text) != tc.want {
				t.Errorf("text = %q, want %q", got.Text, tc.want)
			}
		})
	}
}
