package eventbus

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/dagtest"
)

// TestFromConn_CloseLeavesConnOpen pins the ownership rule: a connection
// handed to FromConn belongs to the caller, so Close must not close it.
func TestFromConn_CloseLeavesConnOpen(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	bus, err := FromConn(context.Background(), nc)
	if err != nil {
		t.Fatalf("FromConn: %v", err)
	}
	_ = bus.Close()
	if nc.IsClosed() {
		t.Fatal("Close closed a connection the bus does not own")
	}
}
