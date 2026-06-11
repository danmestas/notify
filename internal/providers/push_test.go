package providers

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

var _ PushProvider = (*CapturePush)(nil)

func TestCapturePush_RecordsSent(t *testing.T) {
	c := NewCapturePush()
	ctx := context.Background()

	r1, err := c.Send(ctx, PushMessage{Topic: "alerts", Title: "Hi", Body: "one"})
	if err != nil {
		t.Fatalf("send 1: %v", err)
	}
	r2, err := c.Send(ctx, PushMessage{Topic: "ops", Title: "Yo", Body: "two", Priority: 5})
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if r1.ProviderMessageID == "" || r2.ProviderMessageID == "" {
		t.Fatalf("expected non-empty ids, got %q %q", r1.ProviderMessageID, r2.ProviderMessageID)
	}

	sent := c.Sent()
	if len(sent) != 2 {
		t.Fatalf("want 2 sent, got %d", len(sent))
	}
	if sent[0].Topic != "alerts" || sent[0].Title != "Hi" || sent[0].Body != "one" {
		t.Errorf("sent[0] mismatch: %+v", sent[0])
	}
	if sent[1].Topic != "ops" || sent[1].Priority != 5 {
		t.Errorf("sent[1] mismatch: %+v", sent[1])
	}
}

func TestIsTransient_Classification(t *testing.T) {
	plain := errors.New("status 403")
	if IsTransient(plain) {
		t.Error("plain error classified transient, want permanent")
	}

	transient := NewTransientError(errors.New("status 503"))
	if !IsTransient(transient) {
		t.Error("TransientError not classified transient")
	}
	if transient.Error() != "status 503" {
		t.Errorf("Error() = %q, want pass-through message", transient.Error())
	}

	// Classification survives further wrapping.
	wrapped := fmt.Errorf("send push: %w", transient)
	if !IsTransient(wrapped) {
		t.Error("wrapped TransientError lost its classification")
	}
}
