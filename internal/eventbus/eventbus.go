// Package eventbus owns notify's NATS client connection to the dagnats
// sidecar and notify's own private JetStream resources. The raw NATS
// connection and JetStream context are never exposed; callers operate
// through typed methods only.
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Names of notify's private JetStream resources. These belong to notify;
// dagnats owns its own separate buckets and streams which we never touch.
const (
	inboxCountsBucket = "notify_inbox_counts"
	throttleBucket    = "notify_throttle"
	analyticsStream   = "NOTIFY_ANALYTICS"
	analyticsSubject  = "notify.analytics.>"
)

// Bus is notify's handle on the event bus. It holds the client connection
// to the dagnats sidecar plus notify's own JetStream resources.
type Bus struct {
	nc       *nats.Conn
	js       jetstream.JetStream
	kv       jetstream.KeyValue
	throttle jetstream.KeyValue
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

	throttle, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  throttleBucket,
		History: 1,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure kv %q: %w", throttleBucket, err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     analyticsStream,
		Subjects: []string{analyticsSubject},
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure stream %q: %w", analyticsStream, err)
	}

	return &Bus{nc: nc, js: js, kv: kv, throttle: throttle}, nil
}

// Close drains and closes the underlying NATS connection.
func (b *Bus) Close() error {
	if b == nil || b.nc == nil {
		return nil
	}
	b.nc.Close()
	return nil
}

// inboxSubject builds the core-NATS subject for a subscriber's live inbox
// events: notify.inbox.<environmentID>.<subscriberID>.
func inboxSubject(environmentID, subscriberID string) string {
	return "notify.inbox." + environmentID + "." + subscriberID
}

// PublishInbox publishes a live inbox event for (environmentID, subscriberID)
// over core NATS (ephemeral fan-out, not JetStream).
func (b *Bus) PublishInbox(environmentID, subscriberID string, data []byte) error {
	if environmentID == "" || subscriberID == "" {
		return fmt.Errorf("eventbus: PublishInbox: environmentID and subscriberID must be non-empty")
	}
	if err := b.nc.Publish(inboxSubject(environmentID, subscriberID), data); err != nil {
		return fmt.Errorf("eventbus: PublishInbox: %w", err)
	}
	return nil
}

// SubscribeInbox delivers each inbox event for (environmentID, subscriberID)
// to handler until the returned unsubscribe func is called. Uses core NATS,
// so the subscription is ephemeral (no replay).
func (b *Bus) SubscribeInbox(environmentID, subscriberID string, handler func([]byte)) (unsubscribe func() error, err error) {
	if environmentID == "" || subscriberID == "" {
		return nil, fmt.Errorf("eventbus: SubscribeInbox: environmentID and subscriberID must be non-empty")
	}
	sub, err := b.nc.Subscribe(inboxSubject(environmentID, subscriberID), func(m *nats.Msg) {
		handler(m.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: SubscribeInbox: %w", err)
	}
	return sub.Unsubscribe, nil
}

// deliverySubject builds the core-NATS subject for an environment's
// delivery-status events: notify.delivery.<environmentID>.
func deliverySubject(environmentID string) string {
	return "notify.delivery." + environmentID
}

// PublishDelivery publishes a delivery-status event for an environment.
func (b *Bus) PublishDelivery(environmentID string, data []byte) error {
	if environmentID == "" {
		return fmt.Errorf("eventbus: PublishDelivery: environmentID must be non-empty")
	}
	if err := b.nc.Publish(deliverySubject(environmentID), data); err != nil {
		return fmt.Errorf("eventbus: PublishDelivery: %w", err)
	}
	return nil
}

// SubscribeDelivery delivers each delivery-status event for the environment to
// handler until the returned unsubscribe func is called (core NATS, ephemeral).
func (b *Bus) SubscribeDelivery(environmentID string, handler func([]byte)) (unsubscribe func() error, err error) {
	if environmentID == "" {
		return nil, fmt.Errorf("eventbus: SubscribeDelivery: environmentID must be non-empty")
	}
	sub, err := b.nc.Subscribe(deliverySubject(environmentID), func(m *nats.Msg) {
		handler(m.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: SubscribeDelivery: %w", err)
	}
	return sub.Unsubscribe, nil
}

// SubscribeAllDelivery delivers EVERY environment's delivery-status events to
// handler (subject notify.delivery.>). Used by the outbound-webhook consumer.
func (b *Bus) SubscribeAllDelivery(handler func([]byte)) (unsubscribe func() error, err error) {
	sub, err := b.nc.Subscribe("notify.delivery.>", func(m *nats.Msg) {
		handler(m.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: SubscribeAllDelivery: %w", err)
	}
	return sub.Unsubscribe, nil
}

// sanitizeThrottleKey replaces any character not allowed in a NATS KV key
// (alnum, '-', '_', '.', '=') with '_' so an arbitrary caller-supplied key
// is always a valid bucket key.
func sanitizeThrottleKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '=':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ThrottleAllow reports whether an action under `key` is within `limit` per
// `window`, incrementing the fixed-window counter. Best-effort (a small race
// under high concurrency may admit a few extra — acceptable for rate-limiting).
func (b *Bus) ThrottleAllow(key string, limit int, window time.Duration) (allowed bool, err error) {
	bucketKey := sanitizeThrottleKey(key) + "." +
		strconv.FormatInt(time.Now().UnixNano()/int64(window), 10)

	ctx := context.Background()

	var count int
	entry, err := b.throttle.Get(ctx, bucketKey)
	switch {
	case err == nil:
		count, err = strconv.Atoi(string(entry.Value()))
		if err != nil {
			return false, fmt.Errorf("eventbus: throttle parse count for %q: %w", bucketKey, err)
		}
	case errors.Is(err, jetstream.ErrKeyNotFound):
		count = 0
	default:
		return false, fmt.Errorf("eventbus: throttle get %q: %w", bucketKey, err)
	}

	if count >= limit {
		return false, nil
	}

	if _, err := b.throttle.Put(ctx, bucketKey, []byte(strconv.Itoa(count+1))); err != nil {
		return false, fmt.Errorf("eventbus: throttle put %q: %w", bucketKey, err)
	}
	return true, nil
}
