package providers

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// SMSMessage is a channel-agnostic SMS notification. To is the recipient phone
// number (E.164), resolved per-subscriber from the subscriber model's phone
// field (mirrors email's per-subscriber address). From overrides the provider's
// configured sender when non-empty; Body is the rendered message text.
type SMSMessage struct {
	// To is the recipient phone number (the subscriber's phone). Required.
	To string
	// From overrides the provider's configured sender number when non-empty.
	From string
	// Body is the rendered message text.
	Body string
}

// SMSProvider sends an SMS and reports the result. Mirrors ChatProvider: a
// 5xx/timeout/transport fault is wrapped in TransientError so the delivery
// handler can ask the engine to retry per 0001 §10; a 4xx is a permanent
// (plain) error.
type SMSProvider interface {
	Send(ctx context.Context, msg SMSMessage) (Result, error)
}

// CaptureSMS is an in-memory SMSProvider for tests/e2e: it records every sent
// message (thread-safe) and returns a synthetic id. Mirrors CaptureChat.
type CaptureSMS struct {
	mu   sync.Mutex
	sent []SMSMessage
	seq  atomic.Uint64
}

// NewCaptureSMS returns an empty CaptureSMS.
func NewCaptureSMS() *CaptureSMS {
	return &CaptureSMS{}
}

// Send records msg and returns a synthetic provider message id.
func (c *CaptureSMS) Send(_ context.Context, msg SMSMessage) (Result, error) {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	id := c.seq.Add(1)
	return Result{ProviderMessageID: fmt.Sprintf("capture-sms-%d", id)}, nil
}

// Sent returns a copy of the recorded messages.
func (c *CaptureSMS) Sent() []SMSMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SMSMessage, len(c.sent))
	copy(out, c.sent)
	return out
}
