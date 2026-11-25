package providers

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
)

// TestBuildMIME_AttachmentsAreMultipartMixed: the body comes first, whole,
// then one base64 part per file named and dispositioned as an attachment.
func TestBuildMIME_AttachmentsAreMultipartMixed(t *testing.T) {
	pdf := append([]byte("%PDF-1.4\r\n"), bytes.Repeat([]byte{0, 1, 2, 254, 255}, 100)...)
	png := []byte("\x89PNG\r\n\x1a\n")
	for _, c := range []struct {
		name       string
		text, html string
		bodyType   string
	}{
		{"text", "Your invoice", "", "text/plain"},
		{"html", "", "<p>Your invoice</p>", "text/html"},
		{"both", "Your invoice", "<p>Your invoice</p>", "multipart/alternative"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := BuildMIME(EmailMessage{
				From: "a@example.com", To: "b@example.com", Subject: "s", Text: c.text, HTML: c.html,
				Attachments: []Attachment{
					{Filename: "invoice-1042.pdf", ContentType: "application/pdf", Content: pdf},
					{Filename: "café.png", ContentType: "image/png", Content: png},
				},
			})
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
			if err != nil || mt != "multipart/mixed" || params["boundary"] == "" {
				t.Fatalf("Content-Type = %q (%v)", msg.Header.Get("Content-Type"), err)
			}
			mr := multipart.NewReader(msg.Body, params["boundary"])

			body, err := mr.NextRawPart()
			if err != nil {
				t.Fatal(err)
			}
			bt, _, _ := mime.ParseMediaType(body.Header.Get("Content-Type"))
			if bt != c.bodyType {
				t.Fatalf("body part type = %q, want %q", bt, c.bodyType)
			}
			if c.bodyType != "multipart/alternative" {
				got, _ := io.ReadAll(quotedprintable.NewReader(body))
				if want := c.text + c.html; string(got) != want {
					t.Fatalf("body = %q, want %q", got, want)
				}
			}

			for i, want := range []struct {
				name, ctype string
				content     []byte
			}{{"invoice-1042.pdf", "application/pdf", pdf}, {"café.png", "image/png", png}} {
				p, err := mr.NextRawPart()
				if err != nil {
					t.Fatalf("attachment %d: %v", i, err)
				}
				ct, cp, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
				disp, dp, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
				if ct != want.ctype || cp["name"] != want.name || disp != "attachment" || dp["filename"] != want.name {
					t.Fatalf("attachment %d headers = %v", i, p.Header)
				}
				if p.Header.Get("Content-Transfer-Encoding") != "base64" {
					t.Fatalf("attachment %d encoding = %q", i, p.Header.Get("Content-Transfer-Encoding"))
				}
				enc, _ := io.ReadAll(p)
				for _, line := range strings.Split(string(enc), "\r\n") {
					if len(line) > 76 {
						t.Fatalf("attachment %d has a %d-char line", i, len(line))
					}
				}
				got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(enc), "\r\n", ""))
				if err != nil || !bytes.Equal(got, want.content) {
					t.Fatalf("attachment %d content = %d bytes, %v; want %d", i, len(got), err, len(want.content))
				}
			}
			if _, err := mr.NextRawPart(); err != io.EOF {
				t.Fatalf("after the attachments: %v, want EOF", err)
			}
		})
	}
}

// TestBuildMIME_AttachmentGolden pins the bytes of one attachment on a text body.
func TestBuildMIME_AttachmentGolden(t *testing.T) {
	raw, _ := BuildMIME(EmailMessage{
		From: "a@example.com", To: "b@example.com", Subject: "s", Text: "hi",
		Attachments: []Attachment{{Filename: "a.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}},
	})
	want := "From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"_nBOUNDARY\"\r\n\r\n" +
		"--_nBOUNDARY\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nhi\r\n" +
		"--_nBOUNDARY\r\n" +
		"Content-Type: application/pdf; name=a.pdf\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=a.pdf\r\n\r\nJVBERg==\r\n" +
		"--_nBOUNDARY--\r\n"
	// The outer and inner boundaries are the same placeholder after normalising.
	if got := normalizeMIME(raw); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
