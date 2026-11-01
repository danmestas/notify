package engine_test

import (
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// TestFromConn_StopLeavesConnOpen pins the ownership rule: a connection handed
// to FromConn belongs to the caller, so Stop must not close it.
func TestFromConn_StopLeavesConnOpen(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t), engine.InboxOption())
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

// TestFromConn_RefusesConnWithoutProgramInbox: dagnats answers api.p.notify.*
// only under notify's own inbox and drops any other reply unanswered, so an
// engine on a connection without that inbox would hang on every control-plane
// call. FromConn refuses it up front.
func TestFromConn_RefusesConnWithoutProgramInbox(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "_INBOX.notify") {
			t.Fatalf("FromConn on a default-inbox connection: recover() = %v, want a panic naming _INBOX.notify", r)
		}
	}()
	engine.FromConn(nc)
}
