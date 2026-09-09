package emailloop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// spyBus records what a webhook-kind reply publishes.
type spyBus struct {
	mu     sync.Mutex
	envs   []string
	events []ReplyEvent
	err    error
}

func (b *spyBus) PublishDelivery(envID string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	var ev ReplyEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	b.envs = append(b.envs, envID)
	b.events = append(b.events, ev)
	return nil
}

// armReplyScope seeds a verified inbound domain and returns the scope.
func armReplyScope(t *testing.T, f *loopFixture) domain.Scope {
	t.Helper()
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	if _, err := f.st.InboundDomains().Create(ctx, scope, "mail.example.org", "mx.notify.test", 10); err != nil {
		t.Fatalf("inbound domain create: %v", err)
	}
	if err := f.st.InboundDomains().SetStatus(ctx, scope, "mail.example.org", "verified", true); err != nil {
		t.Fatalf("inbound domain verify: %v", err)
	}
	return scope
}

// TestArmReply_MintsMailboxAndWebhookBinding: the reply-to carries a token
// for (notification, subscriber) and the binding is webhook-kind with the
// transaction and workflow the consumer will want back.
func TestArmReply_MintsMailboxAndWebhookBinding(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := context.Background()
	scope := armReplyScope(t, f)

	replyTo, ok, err := f.svc.ArmReply(ctx, scope, "ntf_1", "mention:chat:alpha:7:alice", "quarry-mention", "dmestas", "dev@example.com")
	if err != nil || !ok {
		t.Fatalf("ArmReply = %q, %v, %v", replyTo, ok, err)
	}
	if !strings.HasPrefix(replyTo, "reply+") || !strings.HasSuffix(replyTo, "@mail.example.org") {
		t.Fatalf("replyTo = %q", replyTo)
	}
	rcpt, status := f.svc.CheckRecipient(ctx, replyTo)
	if status != "" {
		t.Fatalf("CheckRecipient on the armed mailbox = %q", status)
	}
	if rcpt.Binding.Kind != KindWebhook || rcpt.Binding.TransactionID != "mention:chat:alpha:7:alice" ||
		rcpt.Binding.WorkflowIdentifier != "quarry-mention" || rcpt.Claims.ThreadID != "ntf_1" {
		t.Fatalf("binding = %+v claims = %+v", rcpt.Binding, rcpt.Claims)
	}
}

// TestArmReply_DeclinesWhenLoopCannotTakeAReply: no verified domain, or
// inbound disabled, means ok=false and no error -- the send goes out without
// promising a reply nobody can make.
func TestArmReply_DeclinesWhenLoopCannotTakeAReply(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := context.Background()

	// No inbound domain at all.
	if rt, ok, err := f.svc.ArmReply(ctx, domain.Scope{OrgID: "o", EnvID: "e"}, "n", "t", "w", "s", "s@e"); ok || err != nil || rt != "" {
		t.Fatalf("no domain: %q %v %v", rt, ok, err)
	}
	// Inbound disabled (no secret, no listener).
	f.svc.secret = ""
	scope := armReplyScope(t, f)
	if rt, ok, err := f.svc.ArmReply(ctx, scope, "n", "t", "w", "s", "s@e"); ok || err != nil || rt != "" {
		t.Fatalf("inbound disabled: %q %v %v", rt, ok, err)
	}
	// A listener counts as inbound being enabled.
	f.svc.EnableListener()
	if _, ok, err := f.svc.ArmReply(ctx, scope, "n", "t", "w", "s", "s@e"); !ok || err != nil {
		t.Fatalf("with listener: ok=%v err=%v", ok, err)
	}
}

// TestAccept_WebhookKindPublishesAndSkipsAgent is the responder split: a
// webhook-kind thread produces one email.reply event on the environment's
// delivery stream and never reaches the agent responder.
func TestAccept_WebhookKindPublishesAndSkipsAgent(t *testing.T) {
	f := newLoopFixture(t, nil)
	bus := &spyBus{}
	f.svc.WithEventPublisher(bus)
	ctx := context.Background()
	scope := armReplyScope(t, f)
	replyTo, _, _ := f.svc.ArmReply(ctx, scope, "ntf_1", "mention:chat:alpha:7:alice", "quarry-mention", "dmestas", "dev@example.com")

	status, err := f.svc.Accept(ctx, InboundEmail{
		From:      "Dev <dev@example.com>",
		To:        replyTo,
		Subject:   "Re: mentioned you",
		Text:      "on it\n\nOn Tue, Quarry wrote:\n> you were mentioned",
		InReplyTo: "<orig@mail.example.org>",
		Headers:   map[string]string{"Message-Id": "<reply@example.com>"},
	})
	if err != nil || status != StatusThreaded {
		t.Fatalf("Accept = %q, %v", status, err)
	}
	if f.dispatchCount() != 0 {
		t.Fatalf("agent responder fired for a webhook-kind thread")
	}
	if len(bus.events) != 1 || bus.envs[0] != "env1" {
		t.Fatalf("events = %+v on %v, want one on env1", bus.events, bus.envs)
	}
	ev := bus.events[0]
	if ev.Type != ReplyEventType || ev.OrgID != "org1" || ev.EnvID != "env1" {
		t.Errorf("routing fields: %+v", ev)
	}
	if ev.SubscriberID != "dmestas" || ev.TransactionID != "mention:chat:alpha:7:alice" || ev.Workflow != "quarry-mention" || ev.NotificationID != "ntf_1" {
		t.Errorf("correlation fields: %+v", ev)
	}
	if ev.Text != "on it" {
		t.Errorf("text = %q, want the quoted original stripped", ev.Text)
	}
	if ev.MessageID != "<reply@example.com>" || ev.InReplyTo != "<orig@mail.example.org>" || ev.ReceivedAt == "" {
		t.Errorf("threading fields: %+v", ev)
	}
}

// TestAccept_AgentKindUntouched: an agent-kind thread still dispatches to the
// responder and publishes nothing, so nothing changes for conversations.
func TestAccept_AgentKindUntouched(t *testing.T) {
	f := newLoopFixture(t, nil)
	bus := &spyBus{}
	f.svc.WithEventPublisher(bus)
	ctx := context.Background()
	scope, threadID, subID := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "dev@example.com")
	token := f.minter.Mint(scope, threadID, subID)

	status, err := f.svc.Accept(ctx, InboundEmail{To: "reply+" + token + "@mail.example.org", Text: "hi"})
	if err != nil || status != StatusThreaded {
		t.Fatalf("Accept = %q, %v", status, err)
	}
	if f.dispatchCount() != 1 || len(bus.events) != 0 {
		t.Fatalf("agent dispatches=%d events=%d; want 1 and 0", f.dispatchCount(), len(bus.events))
	}
}

// TestAccept_WebhookKindWithoutBusIsAnError: a wiring fault must surface as
// a retryable error, not an acknowledged reply that went nowhere.
func TestAccept_WebhookKindWithoutBusIsAnError(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := context.Background()
	scope := armReplyScope(t, f)
	replyTo, _, _ := f.svc.ArmReply(ctx, scope, "ntf_1", "t", "w", "dmestas", "dev@example.com")

	status, err := f.svc.Accept(ctx, InboundEmail{To: replyTo, Text: "hi"})
	if !errors.Is(err, ErrNoEventPublisher) || status != "" {
		t.Fatalf("Accept = %q, %v; want ErrNoEventPublisher and no status", status, err)
	}

	// And a bus that fails is the same shape: the transport should retry.
	f.svc.WithEventPublisher(&spyBus{err: errors.New("nats: connection closed")})
	if _, err := f.svc.Accept(ctx, InboundEmail{To: replyTo, Text: "hi"}); err == nil {
		t.Fatal("publish failure was acknowledged as delivered")
	}
}
