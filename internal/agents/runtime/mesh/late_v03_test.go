package mesh_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/dagtest"
)

// TestDispatch_SynadiaV03LateInfoReply: a v0.3 agent whose first INFO reply
// lands after the detection window is first cached as a2a; the turn's
// acked-no-v2 failure triggers a rescan and the same Dispatch is answered
// through the v0.3 codec.
func TestDispatch_SynadiaV03LateInfoReply(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.late"
	info, _ := json.Marshal(map[string]any{
		"metadata":  map[string]string{"protocol_version": "0.3"},
		"endpoints": []map[string]string{{"name": "prompt", "subject": subject}},
	})
	var infos atomic.Int32
	infoSub, err := nc.Subscribe("$SRV.INFO.agents", func(m *nats.Msg) {
		if infos.Add(1) == 1 {
			time.Sleep(600 * time.Millisecond) // past the 100ms window
		}
		_ = nc.Publish(m.Reply, info)
	})
	if err != nil {
		t.Fatalf("info subscribe: %v", err)
	}
	t.Cleanup(func() { _ = infoSub.Unsubscribe() })
	var prompts atomic.Int32
	promptSub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		if prompts.Add(1) == 1 {
			// First hit is the a2a envelope: ack, then go quiet.
			_ = nc.Publish(m.Reply, chunk("status", "ack"))
			return
		}
		_ = nc.Publish(m.Reply, chunk("response", "PONG"))
		_ = nc.Publish(m.Reply, nil)
	})
	if err != nil {
		t.Fatalf("prompt subscribe: %v", err)
	}
	t.Cleanup(func() { _ = promptSub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	c := dialWith(t, natsURL, mesh.WithDiscoveryWindow(100*time.Millisecond), mesh.WithTimeout(15*time.Second))

	reply, err := c.Dispatch(context.Background(), turn(subject, "hi"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply parts = %+v, want PONG", reply.Parts)
	}
}
