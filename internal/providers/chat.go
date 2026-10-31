package providers

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// ChatMessage is a channel-agnostic chat notification. Address is the
// per-recipient destination a provider needs that is not part of its
// integration credentials: for Telegram it is the chat_id, for Discord it is
// the webhook URL (the webhook is the destination, so Address carries it when
// the subscriber overrides the integration default; an empty Address means
// "use the provider's configured destination").
type ChatMessage struct {
	// Address is the per-recipient destination (Telegram chat_id; Discord
	// webhook URL override). May be empty when the provider's own
	// configuration already fully addresses the destination (Discord).
	Address string
	// Content is the rendered message body.
	Content string
}

// ChatProvider sends a chat notification and reports the result. Mirrors
// PushProvider: a 5xx/timeout/transport fault is wrapped in TransientError so
// the delivery handler can ask the engine to retry per 0001 §10; a 4xx is a
// permanent (plain) error.
type ChatProvider interface {
	Send(ctx context.Context, msg ChatMessage) (Result, error)
}

// CaptureChat is an in-memory ChatProvider for tests/e2e: it records every
// sent message (thread-safe) and returns a synthetic id. Mirrors CapturePush.
type CaptureChat struct {
	mu   sync.Mutex
	sent []ChatMessage
	seq  atomic.Uint64
}

// NewCaptureChat returns an empty CaptureChat.
func NewCaptureChat() *CaptureChat {
	return &CaptureChat{}
}

// Send records msg and returns a synthetic provider message id.
func (c *CaptureChat) Send(_ context.Context, msg ChatMessage) (Result, error) {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	id := c.seq.Add(1)
	return Result{ProviderMessageID: fmt.Sprintf("capture-chat-%d", id)}, nil
}

// Sent returns a copy of the recorded messages.
func (c *CaptureChat) Sent() []ChatMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ChatMessage, len(c.sent))
	copy(out, c.sent)
	return out
}

// Truncate cuts s to at most max runes, replacing the last kept rune with an
// ellipsis when it cuts, so the result is still at most max runes. Each chat
// platform rejects a message over its text limit outright, so notify sends a
// shortened message instead of nothing. Runes approximate the platforms'
// counting (graphemes on Bluesky, UTF-16 units on Telegram); for the text
// notify sends the difference is small.
func Truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}
