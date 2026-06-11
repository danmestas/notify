package providers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// PushMessage is a channel-agnostic push notification addressed to a
// per-subscriber topic (ntfy) or token (FCM/APNs, later breadth).
type PushMessage struct {
	Topic    string
	Title    string
	Body     string
	Priority int // 1 (min) .. 5 (max); 0 leaves the provider default
	Tags     []string
	Markdown bool
}

// PushProvider sends a push notification and reports the result.
type PushProvider interface {
	Send(ctx context.Context, msg PushMessage) (Result, error)
}

// TransientError marks a provider failure as retry-safe (5xx, timeouts,
// transport faults) per 0001 §10. Failures NOT wrapped in TransientError are
// permanent (4xx: bad request, auth, unknown topic) and must not be retried.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// NewTransientError wraps err so callers classify the failure as retry-safe.
func NewTransientError(err error) error { return &TransientError{Err: err} }

// IsTransient reports whether err is (or wraps) a TransientError.
func IsTransient(err error) bool {
	var te *TransientError
	return errors.As(err, &te)
}

// CapturePush is an in-memory PushProvider for tests/e2e: it records every
// sent message (thread-safe) and returns a synthetic id. Mirrors Capture.
type CapturePush struct {
	mu   sync.Mutex
	sent []PushMessage
	seq  atomic.Uint64
}

// NewCapturePush returns an empty CapturePush.
func NewCapturePush() *CapturePush {
	return &CapturePush{}
}

// Send records msg and returns a synthetic provider message id.
func (c *CapturePush) Send(_ context.Context, msg PushMessage) (Result, error) {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	id := c.seq.Add(1)
	return Result{ProviderMessageID: fmt.Sprintf("capture-push-%d", id)}, nil
}

// Sent returns a copy of the recorded messages.
func (c *CapturePush) Sent() []PushMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PushMessage, len(c.sent))
	copy(out, c.sent)
	return out
}
