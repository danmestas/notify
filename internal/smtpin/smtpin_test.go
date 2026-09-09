package smtpin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/emailloop"
	corestore "github.com/danmestas/notify/internal/store"
)

// spyResponder records dispatched turns and can be made to fail.
type spyResponder struct {
	mu    sync.Mutex
	turns []emailloop.InboundTurn
	err   error
}

func (r *spyResponder) Respond(_ context.Context, turn emailloop.InboundTurn) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	r.turns = append(r.turns, turn)
	return turn.ThreadID, nil
}

func (r *spyResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

// fixture is a listening receiver over a real store with one armed reply
// mailbox, driven through the stdlib net/smtp client so what the tests
// exercise is the wire, not the backend interface.
type fixture struct {
	addr      string
	responder *spyResponder
	mailbox   string // reply+<token>@mail.example.org
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := corestore.Open(ctx, filepath.Join(t.TempDir(), "smtpin.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ags := agentstore.New(st)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	minter := emailloop.NewMinter(key)
	responder := &spyResponder{}
	svc := emailloop.New(st, ags, minter, nil, responder, "", slog.Default())

	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	if _, err := st.InboundDomains().Create(ctx, scope, "mail.example.org", "mx.notify.test", 10); err != nil {
		t.Fatalf("inbound domain create: %v", err)
	}
	if err := st.InboundDomains().SetStatus(ctx, scope, "mail.example.org", "verified", true); err != nil {
		t.Fatalf("inbound domain verify: %v", err)
	}
	if _, err := ags.Agents().Create(ctx, scope, agentdomain.Agent{
		Identifier: "helper", Name: "helper", Runtime: agentdomain.RuntimeMesh, Active: true,
	}); err != nil {
		t.Fatalf("agent create: %v", err)
	}
	threadID := corestore.NewULID()
	if err := svc.ArmEmailThread(ctx, scope, "helper", threadID, "sub-1", "dev@example.com", "mail.example.org"); err != nil {
		t.Fatalf("ArmEmailThread: %v", err)
	}
	token := minter.Mint(scope, threadID, "sub-1")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srvCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	server := New(svc, "mx.notify.test", slog.Default())
	go func() { _ = server.Serve(srvCtx, ln) }()

	return &fixture{addr: ln.Addr().String(), responder: responder, mailbox: "reply+" + token + "@mail.example.org"}
}

// dial opens a session and completes HELO.
func (f *fixture) dial(t *testing.T) *smtp.Client {
	t.Helper()
	c, err := smtp.Dial(f.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Hello("sender.example.com"); err != nil {
		t.Fatalf("HELO: %v", err)
	}
	return c
}

// send runs a whole transaction and returns the DATA-phase error, if any.
func (f *fixture) send(t *testing.T, to, body string) error {
	t.Helper()
	c := f.dial(t)
	if err := c.Mail("dev@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return w.Close()
}

const wire = "From: dev@example.com\r\nTo: whatever@elsewhere.example\r\nSubject: Re: hi\r\nContent-Type: text/plain\r\n\r\non it\r\n"

// TestRcpt_RefusesUnknownMailboxBeforeData is the contract in the issue: a
// recipient with no valid, armed token is refused at the envelope, and the
// sender is never allowed to transmit a body.
func TestRcpt_RefusesUnknownMailboxBeforeData(t *testing.T) {
	f := newFixture(t)
	for _, to := range []string{
		"anyone@mail.example.org",
		"reply+notatoken@mail.example.org",
		"reply+" + strings.TrimPrefix(strings.SplitN(f.mailbox, "@", 2)[0], "reply+") + "@unverified.example",
	} {
		c := f.dial(t)
		if err := c.Mail("spammer@example.net"); err != nil {
			t.Fatalf("MAIL: %v", err)
		}
		err := c.Rcpt(to)
		if err == nil {
			t.Fatalf("RCPT %q accepted; want 550 at the envelope", to)
		}
		if !strings.HasPrefix(err.Error(), "550 ") || !strings.Contains(err.Error(), "no such mailbox") {
			t.Errorf("RCPT %q: %v, want a uniform 550 no such mailbox", to, err)
		}
		// DATA must now be refused too: go-smtp requires an accepted RCPT.
		if _, err := c.Data(); err == nil {
			t.Fatalf("DATA accepted after a refused RCPT for %q", to)
		}
	}
	if f.responder.count() != 0 {
		t.Fatalf("responder fired %d times for refused recipients", f.responder.count())
	}
}

// TestData_ThreadsThroughAccept: an accepted recipient's message is parsed
// with the shared parser, pinned to the envelope recipient, and reaches the
// responder with the decoded text.
func TestData_ThreadsThroughAccept(t *testing.T) {
	f := newFixture(t)
	multipart := "From: dev@example.com\r\nTo: Some List <list@example.com>\r\nSubject: Re: hi\r\n" +
		"Content-Type: multipart/alternative; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"caf=C3=A9 fixed\r\n\r\nOn Tue, Quarry wrote:\r\n> earlier\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<p>caf&eacute; fixed</p>\r\n--b--\r\n"
	if err := f.send(t, f.mailbox, multipart); err != nil {
		t.Fatalf("send: %v", err)
	}
	if f.responder.count() != 1 {
		t.Fatalf("responder dispatched %d times, want 1", f.responder.count())
	}
	turn := f.responder.turns[0]
	if turn.Text != "café fixed" {
		t.Errorf("turn text = %q; want the decoded text part with quoted history stripped", turn.Text)
	}
	if turn.AgentIdentifier != "helper" || turn.SubscriberID != "sub-1" {
		t.Errorf("turn = %+v", turn)
	}
}

// TestData_EnvelopeWinsOverHeader: the header To names a list, the envelope
// names the reply mailbox. The envelope is what was verified at RCPT and what
// the message threads on.
func TestData_EnvelopeWinsOverHeader(t *testing.T) {
	f := newFixture(t)
	if err := f.send(t, f.mailbox, wire); err != nil { // header To is whatever@elsewhere.example
		t.Fatalf("send: %v", err)
	}
	if f.responder.count() != 1 {
		t.Fatalf("responder dispatched %d times; the header To must not override the envelope", f.responder.count())
	}
}

// TestData_ResponderDownIsTemporary: a responder failure answers 451 so the
// sending MTA queues and retries, rather than 250 (lost) or 550 (bounced).
func TestData_ResponderDownIsTemporary(t *testing.T) {
	f := newFixture(t)
	f.responder.mu.Lock()
	f.responder.err = errors.New("nats: connection closed")
	f.responder.mu.Unlock()

	err := f.send(t, f.mailbox, wire)
	if err == nil {
		t.Fatal("DATA accepted while the responder was down; the reply is lost")
	}
	if !strings.HasPrefix(err.Error(), "451 ") {
		t.Errorf("DATA error = %v, want 451 so the MTA retries", err)
	}
}

// TestData_OversizeRefused: the same cap as the webhook, enforced on the
// wire with 552.
func TestData_OversizeRefused(t *testing.T) {
	f := newFixture(t)
	// Well-formed lines: a single megabyte line would trip go-smtp's line-length
	// guard first, which drops the connection rather than answering. The size
	// cap is the thing under test, so the body has to be legal mail that is
	// simply too big.
	line := strings.Repeat("x", 900) + "\r\n"
	big := wire + strings.Repeat(line, emailloop.MaxMessageBytes/len(line)+2)
	err := f.send(t, f.mailbox, big)
	if err == nil {
		t.Fatal("oversize DATA accepted")
	}
	if !strings.HasPrefix(err.Error(), "552 ") {
		t.Errorf("DATA error = %v, want 552", err)
	}
	if f.responder.count() != 0 {
		t.Error("responder fired for an oversize message")
	}
}

// TestData_UnparseableIsPermanent: bytes that are not a message get 550, not
// a retry that could never succeed.
func TestData_UnparseableIsPermanent(t *testing.T) {
	f := newFixture(t)
	err := f.send(t, f.mailbox, "\x00\x01 not a message")
	if err == nil {
		t.Fatal("garbage DATA accepted")
	}
	if !strings.HasPrefix(err.Error(), "550 ") {
		t.Errorf("DATA error = %v, want 550", err)
	}
}

// TestNoAuthAdvertised: the token is the authentication, so the receiver
// must not offer AUTH -- offering it would invite credential probing against
// a server that has no credentials.
func TestNoAuthAdvertised(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("receiver advertises AUTH")
	}
}

// TestServe_StopsOnContextCancel: cancelling the context ends Serve and
// closes the listener, so notify's shutdown takes the receiver with it.
func TestServe_StopsOnContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(&spyInbound{}, "x", nil).Serve(ctx, ln) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}

type spyInbound struct{}

func (spyInbound) CheckRecipient(context.Context, string) (emailloop.Recipient, emailloop.AcceptStatus) {
	return emailloop.Recipient{}, emailloop.StatusNoToken
}

func (spyInbound) Accept(context.Context, emailloop.InboundEmail) (emailloop.AcceptStatus, error) {
	return emailloop.StatusNoToken, nil
}
