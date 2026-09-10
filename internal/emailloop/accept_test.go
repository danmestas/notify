package emailloop

import (
	"context"
	"errors"
	"testing"

	corestore "github.com/danmestas/notify/internal/store"
)

// TestAccept_StatusPerDrop pins every outcome of the shared inbound path in
// one place. HandleInbound writes these verbatim, and an SMTP receiver maps
// them to envelope replies, so the set is a contract rather than a detail.
func TestAccept_StatusPerDrop(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := context.Background()
	scope, threadID, _ := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "dev@example.com")
	good := f.token(t, scope, threadID)

	// A token whose thread was never armed: valid signature, no binding.
	unarmed := f.minter.Mint(corestore.NewULID())
	// The armed token presented at a domain that is not verified for the scope.
	other := good

	tests := []struct {
		name string
		mail InboundEmail
		want AcceptStatus
	}{
		{"no reply token", InboundEmail{To: "hello@mail.example.org", Text: "x"}, StatusNoToken},
		{"bad signature", InboundEmail{To: "reply+garbage@mail.example.org", Text: "x"}, StatusInvalidToken},
		{"unverified domain", InboundEmail{To: "reply+" + other + "@elsewhere.example", Text: "x"}, StatusDomainUnverified},
		{"no binding", InboundEmail{To: "reply+" + unarmed + "@mail.example.org", Text: "x"}, StatusNoBinding},
		{"empty body", InboundEmail{To: "reply+" + good + "@mail.example.org"}, StatusEmptyBody},
		{"threaded", InboundEmail{To: "reply+" + good + "@mail.example.org", Text: "on it"}, StatusThreaded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := f.dispatchCount()
			got, err := f.svc.Accept(ctx, tc.mail)
			if err != nil {
				t.Fatalf("Accept returned an error for a drop: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Accept = %q, want %q", got, tc.want)
			}
			dispatched := f.dispatchCount() - before
			if tc.want == StatusThreaded && dispatched != 1 {
				t.Fatalf("threaded but responder dispatched %d times", dispatched)
			}
			if tc.want != StatusThreaded && dispatched != 0 {
				t.Fatalf("dropped as %q but the responder still fired", got)
			}
		})
	}
}

// TestCheckRecipient_RefusesWithoutABody is the property an SMTP receiver
// relies on at RCPT TO: every address-only check runs before any message
// exists, so an unknown mailbox is refused at the envelope and DATA never
// happens. It must agree exactly with Accept.
func TestCheckRecipient_RefusesWithoutABody(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := context.Background()
	scope, threadID, _ := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "dev@example.com")
	good := f.token(t, scope, threadID)
	unarmed := f.minter.Mint(corestore.NewULID())

	for _, tc := range []struct {
		addr string
		want AcceptStatus
	}{
		{"nobody@mail.example.org", StatusNoToken},
		{"reply+@mail.example.org", StatusNoToken},
		{"reply+forged@mail.example.org", StatusInvalidToken},
		{"reply+" + good + "@not-verified.example", StatusDomainUnverified},
		{"reply+" + unarmed + "@mail.example.org", StatusNoBinding},
		{"Dev <reply+" + good + "@mail.example.org>", ""},
	} {
		rcpt, status := f.svc.CheckRecipient(ctx, tc.addr)
		if status != tc.want {
			t.Errorf("CheckRecipient(%q) = %q, want %q", tc.addr, status, tc.want)
		}
		if status == "" {
			if rcpt.Binding.AgentIdentifier != "helper" || rcpt.Binding.PlatformThreadID != threadID {
				t.Errorf("accepted recipient carries wrong binding: %+v", rcpt)
			}
			// Accept must agree with the address-only verdict.
			if got, _ := f.svc.Accept(ctx, InboundEmail{To: tc.addr, Text: "hi"}); got != StatusThreaded {
				t.Errorf("CheckRecipient accepted %q but Accept = %q", tc.addr, got)
			}
		}
	}
	if f.dispatchCount() != 1 {
		t.Fatalf("responder dispatched %d times; only the one Accept call should have", f.dispatchCount())
	}
}

// failingResponder stands in for a responder whose backend is down.
type failingResponder struct{ err error }

func (r failingResponder) Respond(context.Context, InboundTurn) (string, error) { return "", r.err }

// TestAccept_ResponderFailureIsTheOnlyError: a transient responder failure is
// the one outcome a transport should ask the sender to retry, and it is the
// only case Accept reports as an error rather than a status.
func TestAccept_ResponderFailureIsTheOnlyError(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := context.Background()
	scope, threadID, _ := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "dev@example.com")
	good := f.token(t, scope, threadID)

	f.svc.responder = failingResponder{err: errors.New("nats: connection closed")}
	status, err := f.svc.Accept(ctx, InboundEmail{To: "reply+" + good + "@mail.example.org", Text: "hi"})
	if err == nil {
		t.Fatalf("Accept swallowed a responder failure as status %q; the transport would ack a lost turn", status)
	}
	if status != "" {
		t.Errorf("status = %q alongside an error; want empty so callers cannot ack by mistake", status)
	}
}
