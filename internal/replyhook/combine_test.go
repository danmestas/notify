package replyhook

import (
	"context"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

func TestCombine_InvokesBothSubHooks(t *testing.T) {
	var emailCalled, chatCalled bool
	email := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		emailCalled = true
	}
	chat := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		chatCalled = true
	}

	h := Combine(nil, email, chat)
	h(context.Background(), domain.Scope{OrgID: "o1", EnvID: "e1"},
		agentdomain.Conversation{PlatformThreadID: "conv-1"}, "reply")

	if !emailCalled || !chatCalled {
		t.Fatalf("both sub-hooks must run: email=%v chat=%v", emailCalled, chatCalled)
	}
}

func TestCombine_PanicInOneDoesNotPreventOther(t *testing.T) {
	var secondCalled bool
	panicker := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		panic("boom")
	}
	second := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		secondCalled = true
	}

	h := Combine(nil, panicker, second)
	// Must not panic out of Combine, and the second hook must still run.
	h(context.Background(), domain.Scope{}, agentdomain.Conversation{PlatformThreadID: "t"}, "x")

	if !secondCalled {
		t.Fatalf("second hook did not run after first panicked")
	}
}

func TestCombine_EmailOnlyConvChatNoOps(t *testing.T) {
	// Models the real composition: an email-origin conversation. The email hook
	// acts; the chat hook no-ops (its store lookup misses). Here the hooks are
	// fakes keyed on a sentinel thread to model "acts vs no-ops".
	var emailActed, chatActed bool
	emailHook := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		if conv.PlatformThreadID == "email-conv" {
			emailActed = true
		}
	}
	chatHook := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		if conv.PlatformThreadID == "chat-conv" {
			chatActed = true
		}
	}

	h := Combine(nil, emailHook, chatHook)
	h(context.Background(), domain.Scope{}, agentdomain.Conversation{PlatformThreadID: "email-conv"}, "hi")

	if !emailActed {
		t.Fatalf("email hook should act on email-origin conv")
	}
	if chatActed {
		t.Fatalf("chat hook should no-op on email-origin conv")
	}
}

func TestCombine_NilHookSkipped(t *testing.T) {
	var called bool
	real := func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		called = true
	}
	h := Combine(nil, nil, real, nil)
	h(context.Background(), domain.Scope{}, agentdomain.Conversation{}, "x")
	if !called {
		t.Fatalf("non-nil hook must still run alongside nil entries")
	}
}
