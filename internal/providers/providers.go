// Package providers defines channel provider abstractions and email
// implementations. It is stdlib-only (net/smtp, sync, fmt, strings, context).
package providers

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
)

// EmailMessage is a channel-agnostic email to send.
type EmailMessage struct {
	From    string
	To      string
	Subject string
	Text    string
	HTML    string
}

// Result is the provider's send outcome (provider message id, etc.).
type Result struct {
	ProviderMessageID string
}

// EmailProvider sends an email and reports the result.
type EmailProvider interface {
	Send(ctx context.Context, msg EmailMessage) (Result, error)
}

// Capture is an in-memory EmailProvider for tests/e2e: it records every sent
// message (thread-safe) and returns a synthetic id.
type Capture struct {
	mu   sync.Mutex
	sent []EmailMessage
	seq  atomic.Uint64
}

// NewCapture returns an empty Capture.
func NewCapture() *Capture {
	return &Capture{}
}

// Send records msg and returns a synthetic provider message id.
func (c *Capture) Send(_ context.Context, msg EmailMessage) (Result, error) {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	id := c.seq.Add(1)
	return Result{ProviderMessageID: fmt.Sprintf("capture-%d", id)}, nil
}

// Sent returns a copy of the recorded messages.
func (c *Capture) Sent() []EmailMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]EmailMessage, len(c.sent))
	copy(out, c.sent)
	return out
}

// SMTP is a net/smtp EmailProvider. Config from explicit fields.
type SMTP struct {
	Addr     string // host:port
	Username string
	Password string
	Host     string
}

// Send builds an RFC5322 message and dispatches it via net/smtp with PlainAuth.
func (s *SMTP) Send(_ context.Context, msg EmailMessage) (Result, error) {
	auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
	mime := BuildMIME(msg)
	if err := smtp.SendMail(s.Addr, auth, msg.From, []string{msg.To}, mime); err != nil {
		return Result{}, fmt.Errorf("smtp send: %w", err)
	}
	return Result{}, nil
}

// BuildMIME constructs an RFC5322 message with CRLF line endings. When HTML is
// present the Content-Type is text/html and the HTML body is used; otherwise
// it is text/plain with the Text body.
func BuildMIME(msg EmailMessage) []byte {
	contentType := "text/plain; charset=\"utf-8\""
	body := msg.Text
	if msg.HTML != "" {
		contentType = "text/html; charset=\"utf-8\""
		body = msg.HTML
	}

	var b strings.Builder
	writeHeader := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	writeHeader("From", msg.From)
	writeHeader("To", msg.To)
	writeHeader("Subject", msg.Subject)
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", contentType)
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}
