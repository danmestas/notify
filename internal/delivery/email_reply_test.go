package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
)

// spyArmer records the arm request and answers as configured.
type spyArmer struct {
	calls    int
	last     []string
	replyTo  string
	ok       bool
	err      error
	disarmed []string
}

func (a *spyArmer) DisarmReply(_ context.Context, _ domain.Scope, notificationID string) error {
	a.disarmed = append(a.disarmed, notificationID)
	return nil
}

func (a *spyArmer) ArmReply(_ context.Context, _ domain.Scope, notificationID, transactionID, workflowIdentifier, subscriberID, subscriberEmail string) (string, bool, error) {
	a.calls++
	a.last = []string{notificationID, transactionID, workflowIdentifier, subscriberID, subscriberEmail}
	return a.replyTo, a.ok, a.err
}

// replyEnvelope is an email step with the reply opt-in and a static Reply-To
// the armed mailbox must beat.
func replyEnvelope(replyEnabled bool) runenv.InAppEnvelope {
	env := emailEnvelope()
	env.NotificationID = "ntf_1"
	env.TransactionID = "mention:chat:alpha:7:alice"
	env.WorkflowIdentifier = "quarry-mention"
	env.Step.ReplyTo = "static@example.org"
	env.Step.ReplyEnabled = replyEnabled
	return env
}

func sendReplyEnvelope(t *testing.T, h *EmailHandler, env runenv.InAppEnvelope) (json.RawMessage, error) {
	t.Helper()
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return h.HandleEmail(context.Background(), engine.TaskPayload{RunID: "run_r1", StepID: env.Step.StepID, Input: input})
}

// TestHandleEmail_ReplyArmsBeforeSend: with the opt-in, the thread is armed
// before the provider is called and the armed mailbox replaces the static
// Reply-To on the wire.
func TestHandleEmail_ReplyArmsBeforeSend(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	armer := &spyArmer{replyTo: "reply+tok@mail.example.org", ok: true}
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"}).WithReplyArmer(armer)

	if _, err := sendReplyEnvelope(t, h, replyEnvelope(true)); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	if sent[0].ReplyTo != "reply+tok@mail.example.org" {
		t.Errorf("ReplyTo = %q, want the armed mailbox to override the static one", sent[0].ReplyTo)
	}
	if armer.calls != 1 {
		t.Fatalf("armer called %d times", armer.calls)
	}
	want := []string{"ntf_1", "mention:chat:alpha:7:alice", "quarry-mention", "user_1", sent[0].To}
	if strings.Join(armer.last, "|") != strings.Join(want, "|") {
		t.Errorf("armed with %v, want %v", armer.last, want)
	}
}

// TestHandleEmail_ReplyOptOutIsUntouched: without the opt-in nothing is
// armed and the static Reply-To goes out as before.
func TestHandleEmail_ReplyOptOutIsUntouched(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	armer := &spyArmer{replyTo: "reply+tok@mail.example.org", ok: true}
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"}).WithReplyArmer(armer)

	if _, err := sendReplyEnvelope(t, h, replyEnvelope(false)); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	if armer.calls != 0 {
		t.Fatalf("armer called for a step without the opt-in")
	}
	if got := capt.Sent()[0].ReplyTo; got != "static@example.org" {
		t.Errorf("ReplyTo = %q, want the static value untouched", got)
	}
}

// TestHandleEmail_ReplyUnavailableSendsWithout: the loop declining (no
// verified domain) is not an error; the send proceeds with the static
// Reply-To rather than promising a reply that cannot be taken.
func TestHandleEmail_ReplyUnavailableSendsWithout(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"}).WithReplyArmer(&spyArmer{ok: false})

	if _, err := sendReplyEnvelope(t, h, replyEnvelope(true)); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	if got := capt.Sent()[0].ReplyTo; got != "static@example.org" {
		t.Errorf("ReplyTo = %q, want the static value when the loop declines", got)
	}
}

// TestHandleEmail_ReplyArmFailureFailsTheSend: a binding that could not be
// written must not be followed by a message promising a reply to it. This is
// the bind-before-dispatch rule.
func TestHandleEmail_ReplyArmFailureFailsTheSend(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"}).
		WithReplyArmer(&spyArmer{err: errors.New("sqlite: database is locked")})

	// Permanent failures are recorded as an error message row and returned
	// without a Go error -- the handler's convention -- so the assertion is on
	// the persisted outcome and on the provider never being called.
	out, err := sendReplyEnvelope(t, h, replyEnvelope(true))
	if err != nil {
		t.Fatalf("HandleEmail returned a Go error rather than recording the failure: %v", err)
	}
	var msg domain.Message
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Status != "error" || !strings.Contains(msg.ErrorText, "arm reply thread") {
		t.Fatalf("message = status %q error %q; want an error row naming the arm failure", msg.Status, msg.ErrorText)
	}
	if len(capt.Sent()) != 0 {
		t.Fatal("a message was sent promising a reply to a thread that was never bound")
	}
}

// refusingProvider stands in for a provider that rejects the message (#350:
// Resend refusing an over-long Reply-To).
type refusingProvider struct{ err error }

func (p refusingProvider) Send(context.Context, providers.EmailMessage) (providers.Result, error) {
	return providers.Result{}, p.err
}

// TestHandleEmail_ProviderRefusalDisarmsTheThread: a thread armed for a send
// the provider refused is dropped again, so email_threads never holds a
// live mailbox for mail nobody received.
func TestHandleEmail_ProviderRefusalDisarmsTheThread(t *testing.T) {
	s := newStore(t)
	armer := &spyArmer{replyTo: "reply+tok@mail.example.org", ok: true}
	refuse := refusingProvider{err: errors.New("resend: status 422: Invalid reply_to field")}
	h := NewEmail(s, stubResolver{provider: refuse, from: "noreply@example.org"}).WithReplyArmer(armer)

	if _, err := sendReplyEnvelope(t, h, replyEnvelope(true)); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	if got := strings.Join(armer.disarmed, "|"); got != "ntf_1" {
		t.Fatalf("disarmed = %q, want the armed notification ntf_1", got)
	}
}

// TestHandleEmail_SuccessLeavesTheThreadArmed pins the other half: a send that
// went out keeps its binding.
func TestHandleEmail_SuccessLeavesTheThreadArmed(t *testing.T) {
	s := newStore(t)
	armer := &spyArmer{replyTo: "reply+tok@mail.example.org", ok: true}
	h := NewEmail(s, stubResolver{provider: providers.NewCapture(), from: "noreply@example.org"}).WithReplyArmer(armer)

	if _, err := sendReplyEnvelope(t, h, replyEnvelope(true)); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	if len(armer.disarmed) != 0 {
		t.Fatalf("disarmed = %v after a successful send", armer.disarmed)
	}
}
