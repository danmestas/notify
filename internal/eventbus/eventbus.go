// Package eventbus owns notify's NATS client connection to the dagnats
// sidecar and notify's own private JetStream resources. The raw NATS
// connection and JetStream context are never exposed; callers operate
// through typed methods only.
package eventbus

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Names of notify's private JetStream resources. These belong to notify;
// dagnats owns its own separate buckets and streams which we never touch.
const (
	inboxCountsBucket = "notify_inbox_counts"
	analyticsStream   = "NOTIFY_ANALYTICS"
	analyticsSubject  = "notify.analytics.>"
)

// Bus is notify's handle on the event bus. It holds the client connection
// to the dagnats sidecar plus notify's own JetStream resources.
type Bus struct {
	nc *nats.Conn
	js jetstream.JetStream
	kv jetstream.KeyValue
}

// Connect dials natsURL, waits for the connection to be established, and
// ensures notify's JetStream resources exist (idempotent): the
// "notify_inbox_counts" KV bucket and the "NOTIFY_ANALYTICS" stream
// (subjects "notify.analytics.>"). The returned Bus owns the connection;
// the caller must Close it.
func Connect(ctx context.Context, natsURL string) (*Bus, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, fmt.Errorf("eventbus: connect %q: %w", natsURL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: jetstream init: %w", err)
	}

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: inboxCountsBucket,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure kv %q: %w", inboxCountsBucket, err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     analyticsStream,
		Subjects: []string{analyticsSubject},
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure stream %q: %w", analyticsStream, err)
	}

	return &Bus{nc: nc, js: js, kv: kv}, nil
}

// Close drains and closes the underlying NATS connection.
func (b *Bus) Close() error {
	if b == nil || b.nc == nil {
		return nil
	}
	b.nc.Close()
	return nil
}
