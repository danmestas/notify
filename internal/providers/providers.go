// Package providers defines channel provider abstractions and email
// implementations. It is stdlib-only (net/smtp, sync, fmt, strings, context).
package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime/quotedprintable"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EmailMessage is a channel-agnostic email to send.
type EmailMessage struct {
	From    string
	To      string
	Subject string
	Text    string
	HTML    string
	// ReplyTo, when non-empty, sets the Reply-To header (a bare address). The
	// email two-way loop uses it to carry a tokenized reply mailbox
	// (reply+<token>@<inbound-domain>) so a recipient's reply routes back to
	// the originating conversation.
	ReplyTo string
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
	mime, messageID := BuildMIME(msg)
	if err := smtp.SendMail(s.Addr, auth, msg.From, []string{msg.To}, mime); err != nil {
		return Result{}, fmt.Errorf("smtp send: %w", err)
	}
	return Result{ProviderMessageID: messageID}, nil
}

// BuildMIME constructs an RFC 5322 message with CRLF line endings and returns
// it with the Message-ID it minted. When HTML is present the Content-Type is
// text/html and the HTML body is used; otherwise it is text/plain with the
// Text body.
//
// The message carries its own Date and Message-ID rather than leaving them to
// the MTA: a pipe to `sendmail -ti` gets them added by the MTA, but an SMTP
// relay may not, and the reply loop wants a Message-ID it can thread against.
//
// The body is quoted-printable, which is what Fossil's alerts.c does for the
// same reason: 8-bit text and lines over 998 octets do not survive every MTA
// and every pipe, while quoted-printable is safe in all of them and readable
// by anything.
func BuildMIME(msg EmailMessage) (mime []byte, messageID string) {
	messageID = newMessageID(msg.From)

	var b strings.Builder
	writeHeader := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	writeHeader("From", msg.From)
	writeHeader("To", msg.To)
	if msg.ReplyTo != "" {
		writeHeader("Reply-To", msg.ReplyTo)
	}
	writeHeader("Subject", msg.Subject)
	writeHeader("Date", time.Now().UTC().Format(time.RFC1123Z))
	writeHeader("Message-ID", "<"+messageID+">")
	writeHeader("MIME-Version", "1.0")

	if msg.Text != "" && msg.HTML != "" {
		// Both bodies: multipart/alternative, text first and HTML last, since
		// RFC 2046 section 5.1.4 has the last part as the preferred one
		// (#369). A client that will not render HTML shows the text part.
		boundary := newBoundary()
		writeHeader("Content-Type", "multipart/alternative; boundary=\""+boundary+"\"")
		b.WriteString("\r\n")
		for _, part := range []struct{ ctype, body string }{
			{"text/plain; charset=\"utf-8\"", msg.Text},
			{"text/html; charset=\"utf-8\"", msg.HTML},
		} {
			b.WriteString("--" + boundary + "\r\n")
			writeHeader("Content-Type", part.ctype)
			writeHeader("Content-Transfer-Encoding", "quoted-printable")
			b.WriteString("\r\n")
			writeQuotedPrintable(&b, part.body)
			b.WriteString("\r\n")
		}
		b.WriteString("--" + boundary + "--\r\n")
		return []byte(b.String()), messageID
	}

	contentType := "text/plain; charset=\"utf-8\""
	body := msg.Text
	if msg.HTML != "" {
		contentType = "text/html; charset=\"utf-8\""
		body = msg.HTML
	}
	writeHeader("Content-Type", contentType)
	writeHeader("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	writeQuotedPrintable(&b, body)
	return []byte(b.String()), messageID
}

// writeQuotedPrintable encodes body onto b. quotedprintable.Writer emits CRLF
// line endings and soft-wraps at 76.
func writeQuotedPrintable(b *strings.Builder, body string) {
	qp := quotedprintable.NewWriter(b)
	_, _ = qp.Write([]byte(body))
	_ = qp.Close()
}

// newBoundary returns a random multipart boundary. Quoted-printable output
// never contains "--" followed by 32 hex digits and "_n", so the boundary
// cannot collide with a body line.
func newBoundary() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return "_n" + hex.EncodeToString(buf[:])
}

// newMessageID mints a globally unique Message-ID local part @ the sender's
// domain, per RFC 5322 §3.6.4. The domain is taken from From ("Name <a@b>" or
// "a@b"); a From with no domain falls back to a fixed label so the id is still
// well-formed.
func newMessageID(from string) string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	domain := "notify.invalid"
	addr := from
	if i := strings.LastIndexByte(addr, '<'); i >= 0 {
		if j := strings.IndexByte(addr[i:], '>'); j >= 0 {
			addr = addr[i+1 : i+j]
		}
	}
	if at := strings.LastIndexByte(addr, '@'); at >= 0 && at < len(addr)-1 {
		domain = strings.TrimSpace(addr[at+1:])
	}
	return hex.EncodeToString(buf[:]) + "@" + domain
}
