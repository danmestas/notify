package providers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	_ EmailProvider = (*Capture)(nil)
	_ EmailProvider = (*SMTP)(nil)
)

func TestEmailProvider_InterfaceSatisfied(t *testing.T) {
	// Compile-time assertions above; presence here documents intent.
	var _ EmailProvider = NewCapture()
	var _ EmailProvider = &SMTP{}
}

func TestCapture_RecordsSent(t *testing.T) {
	c := NewCapture()
	ctx := context.Background()

	r1, err := c.Send(ctx, EmailMessage{To: "a@x.com", Subject: "Hi", Text: "one"})
	if err != nil {
		t.Fatalf("send 1: %v", err)
	}
	r2, err := c.Send(ctx, EmailMessage{To: "b@x.com", Subject: "Yo", Text: "two"})
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if r1.ProviderMessageID == "" || r2.ProviderMessageID == "" {
		t.Fatalf("expected non-empty ids, got %q %q", r1.ProviderMessageID, r2.ProviderMessageID)
	}

	sent := c.Sent()
	if len(sent) != 2 {
		t.Fatalf("want 2 sent, got %d", len(sent))
	}
	if sent[0].To != "a@x.com" || sent[0].Subject != "Hi" || sent[0].Text != "one" {
		t.Errorf("sent[0] mismatch: %+v", sent[0])
	}
	if sent[1].To != "b@x.com" || sent[1].Subject != "Yo" || sent[1].Text != "two" {
		t.Errorf("sent[1] mismatch: %+v", sent[1])
	}
}

func TestCapture_ConcurrentSendsAreSafe(t *testing.T) {
	c := NewCapture()
	ctx := context.Background()
	const n = 100

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.Send(ctx, EmailMessage{To: "x@y.com", Subject: "s", Text: "t"})
		}()
	}
	wg.Wait()

	if got := len(c.Sent()); got != n {
		t.Fatalf("want %d sent, got %d", n, got)
	}
}

func TestBuildMIME_HeadersAndBody(t *testing.T) {
	textRaw, messageID := BuildMIME(EmailMessage{
		From:    "from@x.com",
		To:      "to@x.com",
		Subject: "Subject Line",
		Text:    "hello body",
	})
	textMIME := string(textRaw)
	for _, want := range []string{
		"From: from@x.com\r\n",
		"To: to@x.com\r\n",
		"Subject: Subject Line\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n",
		"Date: ",
		"Message-ID: <" + messageID + ">\r\n",
		"text/plain",
		"hello body",
	} {
		if !strings.Contains(textMIME, want) {
			t.Errorf("text MIME missing %q in:\n%s", want, textMIME)
		}
	}
	if strings.Contains(textMIME, "text/html") {
		t.Errorf("text MIME should not be html:\n%s", textMIME)
	}
	// Header/body separated by a blank CRLF line.
	if !strings.Contains(textMIME, "\r\n\r\n") {
		t.Errorf("text MIME missing header/body separator:\n%s", textMIME)
	}

	htmlRaw, _ := BuildMIME(EmailMessage{
		From:    "from@x.com",
		To:      "to@x.com",
		Subject: "HTML",
		Text:    "plain fallback",
		HTML:    "<p>hi</p>",
	})
	htmlMIME := string(htmlRaw)
	if !strings.Contains(htmlMIME, "text/html") {
		t.Errorf("html MIME missing text/html:\n%s", htmlMIME)
	}
	if !strings.Contains(htmlMIME, "<p>hi</p>") {
		t.Errorf("html MIME missing html body:\n%s", htmlMIME)
	}
	if strings.Contains(htmlMIME, "text/plain") {
		t.Errorf("html MIME should not be text/plain:\n%s", htmlMIME)
	}
}

// TestBuildMIME_MessageIDAndEncoding pins the parts a pipe to an MTA depends
// on: a well-formed Message-ID at the sender's domain, an RFC 5322 Date, and a
// quoted-printable body so 8-bit text and long lines survive the pipe.
func TestBuildMIME_MessageIDAndEncoding(t *testing.T) {
	raw, id := BuildMIME(EmailMessage{
		From:    "Quarry <forge@example.org>",
		To:      "to@x.com",
		Subject: "S",
		Text:    "caf\u00e9 " + strings.Repeat("x", 1200),
	})
	mime := string(raw)
	if !strings.HasSuffix(id, "@example.org") {
		t.Errorf("Message-ID = %q, want the sender's domain", id)
	}
	if strings.Contains(mime, "caf\u00e9") {
		t.Errorf("8-bit body was written raw; want quoted-printable")
	}
	if !strings.Contains(mime, "caf=C3=A9") {
		t.Errorf("body not quoted-printable encoded:\n%s", mime)
	}
	for _, line := range strings.Split(mime, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line of %d octets exceeds the RFC 5322 limit; soft-wrap missing", len(line))
		}
	}
	dateLine := ""
	for _, line := range strings.Split(mime, "\r\n") {
		if strings.HasPrefix(line, "Date: ") {
			dateLine = strings.TrimPrefix(line, "Date: ")
		}
	}
	if _, err := time.Parse(time.RFC1123Z, dateLine); err != nil {
		t.Errorf("Date header %q is not RFC 1123Z: %v", dateLine, err)
	}
}
