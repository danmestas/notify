package dagtest_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/notify/internal/dagtest"
)

// TestStartSidecar_InProcess proves the sidecar needs no dagnats binary: with
// PATH empty, HOME pointed at an empty dir and DAGNATS_BIN naming nothing, it
// still serves NATS + JetStream (with dagnats' streams provisioned) and the
// NATS control plane.
func TestStartSidecar_InProcess(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DAGNATS_BIN", "/nonexistent/dagnats")

	natsURL := dagtest.StartSidecar(t)

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect %s: %v", natsURL, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.Stream(ctx, "DEAD_LETTERS"); err != nil {
		t.Fatalf("dagnats streams not provisioned: %v", err)
	}

	// api.runs.get answers with an {"error":...} envelope for an unknown
	// run, which is enough to prove the control plane is serving.
	msg, err := nc.RequestWithContext(ctx, "api.runs.get", []byte("no-such-run"))
	if err != nil {
		t.Fatalf("api.runs.get: %v", err)
	}
	if len(msg.Data) == 0 {
		t.Fatal("api.runs.get replied with an empty body")
	}
}
