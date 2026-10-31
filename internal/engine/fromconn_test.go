package engine_test

import (
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// TestFromConn_StopLeavesConnOpen pins the ownership rule: a connection handed
// to FromConn belongs to the caller, so Stop must not close it.
func TestFromConn_StopLeavesConnOpen(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	e := engine.FromConn(nc)
	if err := e.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	_ = e.Stop()
	if nc.IsClosed() {
		t.Fatal("Stop closed a connection the engine does not own")
	}
}
