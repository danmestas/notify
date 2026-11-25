package providers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// TestSMTP_AttachmentWithLongNonASCIIFilename sends a 200-byte (the longest
// allowed) non-ASCII filename through SMTP: the RFC 2231-encoded headers stay
// under the 998-octet line limit and the name decodes back intact.
func TestSMTP_AttachmentWithLongNonASCIIFilename(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = fmt.Fprintf(c, "%s\r\n", s) }
		say("220 test ready")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					say("250 queued")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250 test")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				inData = true
				say("354 go")
			case cmd == "QUIT":
				say("221 bye")
				got <- data.String()
				return
			default:
				say("500 what")
			}
		}
	}()

	name := strings.Repeat("é", 100) // 200 bytes of UTF-8
	if len(name) != 200 {
		t.Fatalf("test name is %d bytes", len(name))
	}
	content := []byte("%PDF-1.4 smtp")
	s := &SMTP{Addr: ln.Addr().String(), Host: "127.0.0.1"}
	if _, err := s.Send(context.Background(), EmailMessage{
		From: "a@x.test", To: "b@x.test", Subject: "Invoice", Text: "attached",
		Attachments: []Attachment{{Filename: name, ContentType: "application/pdf", Content: content}},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var raw string
	select {
	case raw = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw QUIT")
	}
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a %d-octet line exceeds the 998 limit: %.80q...", len(line), line)
		}
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	if _, err := mr.NextRawPart(); err != nil {
		t.Fatal(err)
	}
	p, err := mr.NextRawPart()
	if err != nil {
		t.Fatal(err)
	}
	_, dp, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil || dp["filename"] != name {
		t.Fatalf("filename = %q (%v), want the 200-byte name", dp["filename"], err)
	}
	_, cp, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
	if cp["name"] != name {
		t.Fatalf("name = %q", cp["name"])
	}
	if !bytes.Contains([]byte(raw), []byte("filename*")) {
		t.Fatal("a non-ASCII filename was not RFC 2231 encoded")
	}
}
