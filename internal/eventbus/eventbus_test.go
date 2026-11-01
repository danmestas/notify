package eventbus

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// inspectJS opens an independent JetStream context against the sidecar so
// tests verify resources from the server's perspective, not via the Bus's
// private handles.
func inspectJS(t *testing.T, natsURL string) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("inspect connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("inspect jetstream: %v", err)
	}
	return js
}

func TestBus_Connect_CreatesAnalyticsStream(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	js := inspectJS(t, natsURL)
	stream, err := js.Stream(ctx, analyticsStream)
	if err != nil {
		t.Fatalf("Stream(%q): %v", analyticsStream, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if !slices.Contains(info.Config.Subjects, analyticsSubject) {
		t.Fatalf("stream %q subjects = %v, want to contain %q",
			analyticsStream, info.Config.Subjects, analyticsSubject)
	}
}

func TestBus_Connect_Idempotent(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus1, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	defer bus1.Close()

	bus2, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("second Connect (idempotent): %v", err)
	}
	defer bus2.Close()
}
