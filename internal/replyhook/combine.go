// Package replyhook composes multiple post-turn reply callbacks into one.
//
// ingress exposes a single reply-hook slot, but notify has two two-way loops
// (email and chat) that each need to act on an agent reply. Combine wraps them
// into one hook that invokes every sub-hook best-effort: each loop's hook
// no-ops when the conversation is not its binding, so they compose cleanly, and
// a panic in one hook is recovered and logged so it never fails the turn or
// blocks the others.
package replyhook

import (
	"context"
	"log/slog"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

// Hook is the post-turn reply callback shape (matches ingress.ReplyHook).
type Hook func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string)

// Combine returns one callback that calls every non-nil hook in order. Each
// call is wrapped in a recover so a panicking hook neither fails the turn nor
// prevents the remaining hooks from running. log records recovered panics; a
// nil log uses slog.Default.
//
// The return type is the literal callback signature (an unnamed func type)
// rather than Hook, so it is directly assignable to ingress.ReplyHook (a
// different named type with the same underlying signature) at the wiring site.
func Combine(log *slog.Logger, hooks ...Hook) func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
		for _, h := range hooks {
			if h == nil {
				continue
			}
			runOne(log, h, ctx, scope, conv, text)
		}
	}
}

// runOne invokes one hook with a panic guard.
func runOne(log *slog.Logger, h Hook, ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("replyhook: sub-hook panicked", "panic", r, "thread", conv.PlatformThreadID)
		}
	}()
	h(ctx, scope, conv, text)
}
