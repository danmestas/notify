// Package smtpin is the embedded SMTP receiver: with it listening, the MX host
// for an inbound domain can be notify itself, and the reply loop is
// self-hosted with nothing else installed.
//
// It is not a mail server. Every address it will accept is a reply mailbox,
// reply+<token>@<inbound-domain>, and the token is an HMAC the loop already
// mints and verifies. So a recipient that is not a valid, armed reply address
// is refused at RCPT TO, before DATA: the message body is never read, and
// there is no inbox to fill. That is the whole spam and abuse story. No DKIM
// or SPF: the token authenticates the person and the thread, which is more
// than either header proves. No relaying, no local delivery, no queue.
//
// Accepted messages go through the same emailloop.ParseMessage and
// emailloop.Accept the JSON webhook uses, so the two doors cannot disagree
// about what a message contained or where it went.
package smtpin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/danmestas/notify/internal/emailloop"
)

// Inbound is the slice of emailloop.Service the receiver needs.
type Inbound interface {
	CheckRecipient(ctx context.Context, addr string) (emailloop.Recipient, emailloop.AcceptStatus)
	Accept(ctx context.Context, payload emailloop.InboundEmail) (emailloop.AcceptStatus, error)
}

// Per-command bound. A reply mailbox check is a signature verify and two
// indexed reads; parsing plus dispatch is bounded by the message size cap.
const commandTimeout = 30 * time.Second

// Server is one SMTP listener over an Inbound.
type Server struct {
	inbound Inbound
	log     *slog.Logger
	srv     *smtp.Server
}

// New builds a receiver announcing itself as domain in the banner (the MX
// host name, or the machine's). It accepts plaintext only in this version:
// a reply carries no secret beyond its single-thread, single-subscriber token,
// and STARTTLS is the obvious next step once a certificate is in hand.
func New(inbound Inbound, domain string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{inbound: inbound, log: log}
	srv := smtp.NewServer(smtp.BackendFunc(s.newSession))
	srv.Domain = domain
	// The same cap the webhook and ParseMessage enforce, so no door is bigger
	// than another. go-smtp answers 552 on its own when a client exceeds it.
	srv.MaxMessageBytes = emailloop.MaxMessageBytes
	// A reply goes to exactly one reply mailbox. go-smtp answers 452 to a
	// second RCPT on its own.
	srv.MaxRecipients = 1
	srv.ReadTimeout = commandTimeout
	srv.WriteTimeout = commandTimeout
	srv.ErrorLog = slogPrinter{log: log}
	s.srv = srv
	return s
}

// Serve accepts connections on ln until ctx is cancelled, then shuts down
// gracefully, giving in-flight messages a bounded window to finish.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Closing ln directly as well as calling Shutdown: go-smtp only learns
	// about ln once Serve is inside it, so a context cancelled before that
	// point would leave Accept blocked forever with nothing to close it.
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if err := s.srv.Shutdown(sctx); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			_ = s.srv.Close()
		}
		_ = ln.Close()
	}()
	s.log.Info("smtpin: listening", "addr", ln.Addr().String(), "domain", s.srv.Domain)
	err := s.srv.Serve(ln)
	if ctx.Err() != nil {
		// Stopped on request: a closed-listener error is the expected shape.
		return nil
	}
	if err != nil && !errors.Is(err, smtp.ErrServerClosed) {
		return fmt.Errorf("smtpin: serve: %w", err)
	}
	return nil
}

// ListenAndServe binds addr and calls Serve.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("smtpin: listen %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

func (s *Server) newSession(c *smtp.Conn) (smtp.Session, error) {
	remote := ""
	if nc := c.Conn(); nc != nil && nc.RemoteAddr() != nil {
		remote = nc.RemoteAddr().String()
	}
	return &session{server: s, remote: remote, hello: c.Hostname()}, nil
}

// session is one SMTP transaction. go-smtp enforces command ordering (MAIL
// before RCPT before DATA) and the recipient cap; the session only decides
// whether a mailbox exists and what to do with a message for it.
type session struct {
	server *Server
	remote string
	hello  string

	from string
	to   string
	rcpt emailloop.Recipient
}

// The one refusal a sender sees. Which check failed is logged, not told: an
// address prober must not learn whether a token was well-formed, expired, or
// merely unarmed.
var errNoSuchMailbox = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such mailbox"}

// errTryLater is the retryable answer for a responder that is down: the
// sending MTA queues and retries, which is exactly what should happen.
var errTryLater = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "try again later"}

// errNotParseable is permanent: retrying the same bytes cannot help.
var errNotParseable = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message could not be parsed"}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

// Rcpt is where the receiver earns its keep: every address-only check runs
// here, and a message for anything but a live reply mailbox is refused before
// the sender is allowed to transmit it.
func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	rcpt, status := s.server.inbound.CheckRecipient(ctx, to)
	if status != "" {
		// CheckRecipient logged the reason; this adds who asked.
		s.server.log.Warn("smtpin: recipient refused", "to", to, "from", s.from, "remote", s.remote, "reason", string(status))
		return errNoSuchMailbox
	}
	s.to, s.rcpt = to, rcpt
	s.server.log.Info("smtpin: recipient accepted", "to", to, "from", s.from, "remote", s.remote, "thread", rcpt.Claims.ThreadID)
	return nil
}

// Data reads the message, parses it through the shared parser, pins the
// envelope recipient to what RCPT TO actually said, and hands it to Accept.
func (s *session) Data(r io.Reader) error {
	if s.to == "" {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "RCPT TO first"}
	}
	// go-smtp already enforces MaxMessageBytes; the extra byte here is a
	// belt for its braces, so ParseMessage's own cap is never the first to see
	// an oversize body.
	raw, err := io.ReadAll(io.LimitReader(r, emailloop.MaxMessageBytes+1))
	// The Session contract: r must be consumed before Data returns, or the
	// server has no clean point at which to write the reply and drops the
	// connection -- the sender then sees a broken pipe instead of a 552 it
	// could act on. The drain is bounded because go-smtp's own reader stops
	// at MaxMessageBytes.
	_, _ = io.Copy(io.Discard, r)
	if errors.Is(err, smtp.ErrDataTooLarge) || len(raw) > emailloop.MaxMessageBytes {
		s.server.log.Warn("smtpin: message too large, refused", "to", s.to, "from", s.from, "remote", s.remote)
		return smtp.ErrDataTooLarge
	}
	if err != nil {
		return errTryLater
	}

	parsed, err := emailloop.ParseMessage(raw)
	if err != nil {
		s.server.log.Warn("smtpin: message not parseable, refused", "to", s.to, "from", s.from, "remote", s.remote, "err", err)
		return errNotParseable
	}
	// The header To can say anything; the envelope is who the sender actually
	// addressed, and it is what Rcpt already verified.
	parsed.EnvelopeTo = s.to

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	status, err := s.server.inbound.Accept(ctx, parsed)
	if err != nil {
		s.server.log.Error("smtpin: accept failed, asking sender to retry", "to", s.to, "from", s.from, "remote", s.remote, "err", err)
		return errTryLater
	}
	s.server.log.Info("smtpin: message accepted", "to", s.to, "from", s.from, "remote", s.remote, "status", string(status), "bytes", len(raw))
	return nil
}

func (s *session) Reset() {
	s.from, s.to, s.rcpt = "", "", emailloop.Recipient{}
}

func (s *session) Logout() error { return nil }

// slogPrinter adapts go-smtp's Logger to slog so the server's own diagnostics
// land in the same stream as everything else.
type slogPrinter struct{ log *slog.Logger }

func (p slogPrinter) Printf(format string, v ...any) {
	p.log.Warn("smtpin: " + fmt.Sprintf(format, v...))
}

func (p slogPrinter) Println(v ...any) {
	p.log.Warn("smtpin: " + fmt.Sprint(v...))
}
