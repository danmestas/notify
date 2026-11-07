package eventbus

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// TestPublishDelivery_SetsMsgID asserts the stored message carries a
// Nats-Msg-Id derived from the event's message id and status, and that a
// duplicate publish lands once in the stream.
func TestPublishDelivery_SetsMsgID(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	ev := []byte(`{"ID":"m1","Status":"sent","OrgID":"o1","EnvID":"env1"}`)
	for i := 0; i < 2; i++ {
		if err := bus.PublishDelivery("env1", ev); err != nil {
			t.Fatalf("PublishDelivery #%d: %v", i, err)
		}
	}
	// A different status of the same message is a distinct event.
	if err := bus.PublishDelivery("env1", []byte(`{"ID":"m1","Status":"read"}`)); err != nil {
		t.Fatalf("PublishDelivery(read): %v", err)
	}

	js := inspectJS(t, natsURL)
	stream, err := js.Stream(ctx, deliveryStream)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.State.Msgs != 2 {
		t.Fatalf("stream holds %d msgs, want 2 (duplicate dropped)", info.State.Msgs)
	}
	raw, err := stream.GetMsg(ctx, info.State.FirstSeq)
	if err != nil {
		t.Fatalf("GetMsg: %v", err)
	}
	if got := raw.Header.Get(jetstream.MsgIDHeader); got != "m1:sent" {
		t.Fatalf("Nats-Msg-Id = %q, want %q", got, "m1:sent")
	}
}

func TestDeliveryEventKey(t *testing.T) {
	if got := DeliveryEventKey([]byte(`{"ID":"m1","Status":"sent"}`)); got != "m1:sent" {
		t.Fatalf("with id: %q", got)
	}
	a := DeliveryEventKey([]byte(`{"event":"x"}`))
	b := DeliveryEventKey([]byte(`{"event":"x"}`))
	c := DeliveryEventKey([]byte(`{"event":"y"}`))
	if a == "" || a != b || a == c {
		t.Fatalf("hash fallback not deterministic/distinct: %q %q %q", a, b, c)
	}
	if got := DeliveryEventKey([]byte(`not json`)); got == "" {
		t.Fatal("unparseable payload must still key by content hash")
	}
}
