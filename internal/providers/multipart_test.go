package providers

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
)

// TestBuildMIME_BothBodiesAreMultipartAlternative is #369: with a text and
// an HTML body the message is multipart/alternative, text first and HTML
// last (RFC 2046 5.1.4: the last part is preferred), each part decodable to
// its input.
func TestBuildMIME_BothBodiesAreMultipartAlternative(t *testing.T) {
	text := "Line one with café\n\n- item\n" + strings.Repeat("long ", 40)
	html := "<p>Line one with <b>café</b></p><ul><li>item</li></ul>"
	raw, _ := BuildMIME(EmailMessage{From: "a@example.com", To: "b@example.com", Subject: "s", Text: text, HTML: html})

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q (%v), want multipart/alternative with a boundary", msg.Header.Get("Content-Type"), err)
	}
	if msg.Header.Get("Content-Transfer-Encoding") != "" {
		t.Fatal("a multipart message must not carry a top-level transfer encoding")
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	want := []struct{ ctype, body string }{{"text/plain", text}, {"text/html", html}}
	for i, w := range want {
		p, err := mr.NextRawPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		pt, pp, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if pt != w.ctype || pp["charset"] != "utf-8" || p.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Fatalf("part %d headers = %v", i, p.Header)
		}
		got, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatalf("part %d decode: %v", i, err)
		}
		// quoted-printable output uses CRLF line endings.
		if strings.ReplaceAll(string(got), "\r\n", "\n") != w.body {
			t.Fatalf("part %d body = %q, want %q", i, got, w.body)
		}
	}
	if _, err := mr.NextRawPart(); err != io.EOF {
		t.Fatalf("after two parts: %v, want EOF", err)
	}
}

// TestBuildMIME_OneBodyIsStillSinglePart: only one body set produces the
// same single-part message as before #369.
func TestBuildMIME_OneBodyIsStillSinglePart(t *testing.T) {
	for _, c := range []struct {
		name  string
		msg   EmailMessage
		ctype string
	}{
		{"text", EmailMessage{From: "a@x.com", To: "b@x.com", Subject: "s", Text: "hi"}, "text/plain"},
		{"html", EmailMessage{From: "a@x.com", To: "b@x.com", Subject: "s", HTML: "<p>hi</p>"}, "text/html"},
	} {
		raw, _ := BuildMIME(c.msg)
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
		if mt != c.ctype || params["charset"] != "utf-8" || msg.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Fatalf("%s: headers = %v", c.name, msg.Header)
		}
		if bytes.Contains(raw, []byte("multipart")) {
			t.Fatalf("%s: single body produced multipart", c.name)
		}
	}
}
