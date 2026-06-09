package providers

import (
	"context"
	"strings"
	"sync"
	"testing"
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
	textMIME := string(BuildMIME(EmailMessage{
		From:    "from@x.com",
		To:      "to@x.com",
		Subject: "Subject Line",
		Text:    "hello body",
	}))
	for _, want := range []string{
		"From: from@x.com\r\n",
		"To: to@x.com\r\n",
		"Subject: Subject Line\r\n",
		"MIME-Version: 1.0\r\n",
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

	htmlMIME := string(BuildMIME(EmailMessage{
		From:    "from@x.com",
		To:      "to@x.com",
		Subject: "HTML",
		Text:    "plain fallback",
		HTML:    "<p>hi</p>",
	}))
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
