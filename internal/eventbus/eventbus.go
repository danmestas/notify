// Package eventbus owns notify's NATS client connection to the dagnats
// sidecar and notify's own private JetStream resources. The raw NATS
// connection and JetStream context are never exposed; callers operate
// through typed methods only.
package eventbus

import (
	"context"
	"encoding/json"
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
	inboxCountsBucket  = "notify_inbox_counts"
	throttleBucket     = "notify_throttle"
	digestBucket       = "notify_digest"
	analyticsStream    = "NOTIFY_ANALYTICS"
	analyticsSubject   = "notify.analytics.>"
	deliveryStream     = "NOTIFY_DELIVERY"
	deliverySubjectAll = "notify.delivery.>"

	// deliveryStreamMaxAge bounds how long delivery-status events are
	// retained on the NOTIFY_DELIVERY stream. Durable consumers (e.g.
	// webhooksout) must catch up within this window after downtime;
	// 7 days covers any realistic outage.
	deliveryStreamMaxAge = 7 * 24 * time.Hour

	// maxThrottleBucketTTL is the per-key TTL on the notify_throttle bucket.
	// Throttle counters are keyed per (compositeKey, windowEpoch); without a
	// TTL they accumulate indefinitely — one key per distinct window boundary
	// per subscriber/workflow pair, forever. 30 days covers the largest
	// practical throttle window; the extra day of grace ensures a counter is
	// never evicted mid-window even for a full 30-day window configuration.
	maxThrottleBucketTTL = 31 * 24 * time.Hour
)

// Bus is notify's handle on the event bus. It holds the client connection
// to the dagnats sidecar plus notify's own JetStream resources.
type Bus struct {
	nc       *nats.Conn
	js       jetstream.JetStream
	kv       jetstream.KeyValue
	throttle jetstream.KeyValue
	digest   jetstream.KeyValue
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
		TTL:     maxThrottleBucketTTL,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure kv %q: %w", throttleBucket, err)
	}

	digest, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  digestBucket,
		History: 1,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure kv %q: %w", digestBucket, err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     analyticsStream,
		Subjects: []string{analyticsSubject},
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure stream %q: %w", analyticsStream, err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     deliveryStream,
		Subjects: []string{deliverySubjectAll},
		MaxAge:   deliveryStreamMaxAge,
		Storage:  jetstream.FileStorage,
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: ensure stream %q: %w", deliveryStream, err)
	}

	return &Bus{nc: nc, js: js, kv: kv, throttle: throttle, digest: digest}, nil
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

// PublishDelivery publishes a delivery-status event for an environment onto
// the durable NOTIFY_DELIVERY stream (JetStream, at-least-once) so it
// survives restarts and consumer downtime. Core-NATS subscribers on the same
// subject (SubscribeDelivery, SubscribeAllDelivery) still receive it live —
// JetStream publish is a normal NATS publish that the stream also captures.
func (b *Bus) PublishDelivery(environmentID string, data []byte) error {
	if environmentID == "" {
		return fmt.Errorf("eventbus: PublishDelivery: environmentID must be non-empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.js.Publish(ctx, deliverySubject(environmentID), data); err != nil {
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

// ConsumeAllDeliveryDurable starts (or resumes) a durable JetStream consumer
// named durable on the NOTIFY_DELIVERY stream, delivering every
// environment's delivery-status events (subject notify.delivery.>) to
// handler at least once.
//
// Ack/Nak semantics: when handler returns nil the message is Acked, telling
// the server to advance past it and never redeliver it. When handler returns
// an error the message is Naked, telling the server to redeliver it
// immediately (up to MaxDeliver=5 attempts, AckWait=30s between attempts if
// redelivery is instead triggered by a timeout rather than an explicit Nak).
// Because the consumer is durable, restarting ConsumeAllDeliveryDurable with
// the SAME durable name resumes from the last acked position on the
// server — events published while no consumer was running are delivered on
// the next start, and already-acked events are not redelivered.
//
// The returned stop func drains the consumer: it stops pulling new messages
// but lets any message already handed to handler finish before returning.
func (b *Bus) ConsumeAllDeliveryDurable(ctx context.Context, durable string, handler func(data []byte) error) (stop func() error, err error) {
	if durable == "" {
		return nil, fmt.Errorf("eventbus: ConsumeAllDeliveryDurable: durable must be non-empty")
	}

	cons, err := b.js.CreateOrUpdateConsumer(ctx, deliveryStream, jetstream.ConsumerConfig{
		Durable:        durable,
		FilterSubjects: []string{deliverySubjectAll},
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        30 * time.Second,
		MaxDeliver:     5,
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: ConsumeAllDeliveryDurable: ensure consumer %q: %w", durable, err)
	}

	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		if handler(msg.Data()) == nil {
			_ = msg.Ack()
		} else {
			_ = msg.Nak()
		}
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: ConsumeAllDeliveryDurable: consume %q: %w", durable, err)
	}

	return func() error {
		consumeCtx.Drain()
		return nil
	}, nil
}

// buildStreamSubject builds the dagnats run-stream subject the AI workflow
// builder's agent-loop step publishes its live Events on: stream.<runID>.build.
// "build" is the builder's single agent-loop step id (aibuilder.AgentLoopStepID);
// the dagnats streaming convention is stream.<runID>.<stepID>.
func buildStreamSubject(runID string) string {
	return "stream." + runID + ".build"
}

// SubscribeBuildStream delivers each AI-builder streaming Event for the run to
// handler until the returned unsubscribe func is called. Uses core NATS, so the
// subscription is ephemeral (no replay) — the console subscribes for the life of
// one generate request and drops it when the request ends.
func (b *Bus) SubscribeBuildStream(runID string, handler func([]byte)) (unsubscribe func() error, err error) {
	if runID == "" {
		return nil, fmt.Errorf("eventbus: SubscribeBuildStream: runID must be non-empty")
	}
	sub, err := b.nc.Subscribe(buildStreamSubject(runID), func(m *nats.Msg) {
		handler(m.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: SubscribeBuildStream: %w", err)
	}
	return sub.Unsubscribe, nil
}

// sanitizeKey replaces any character not allowed in a NATS KV key
// (alnum, '-', '_', '.', '=') with '_' so an arbitrary caller-supplied key
// is always a valid bucket key.
func sanitizeKey(key string) string {
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
	bucketKey := sanitizeKey(key) + "." +
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

// digestListKey returns the KV key holding the accumulated event list for key.
func digestListKey(key string) string { return sanitizeKey(key) + ".events" }

// digestClaimKey returns the KV key used to atomically claim the digest window.
func digestClaimKey(key string) string { return sanitizeKey(key) + ".claim" }

// DigestAppend appends one event payload to the accumulator list at key.
func (b *Bus) DigestAppend(key string, event json.RawMessage) error {
	ctx := context.Background()
	listKey := digestListKey(key)

	var events []json.RawMessage
	entry, err := b.digest.Get(ctx, listKey)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(entry.Value(), &events); uerr != nil {
			return fmt.Errorf("eventbus: digest unmarshal %q: %w", listKey, uerr)
		}
	case errors.Is(err, jetstream.ErrKeyNotFound):
		// No list yet; start a fresh one.
	default:
		return fmt.Errorf("eventbus: digest get %q: %w", listKey, err)
	}

	events = append(events, event)
	data, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("eventbus: digest marshal %q: %w", listKey, err)
	}
	if _, err := b.digest.Put(ctx, listKey, data); err != nil {
		return fmt.Errorf("eventbus: digest put %q: %w", listKey, err)
	}
	return nil
}

// DigestClaim atomically claims the digest window for key: returns isOwner=true
// for the FIRST caller (which should start the windowed run), false for
// subsequent callers within the window. Uses KV Create (atomic) on a claim key.
func (b *Bus) DigestClaim(key string) (isOwner bool, err error) {
	ctx := context.Background()
	claimKey := digestClaimKey(key)

	if _, err := b.digest.Create(ctx, claimKey, []byte("1")); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, fmt.Errorf("eventbus: digest claim %q: %w", claimKey, err)
	}
	return true, nil
}

// DigestDrain returns all accumulated events for key and clears the list + claim.
func (b *Bus) DigestDrain(key string) ([]json.RawMessage, error) {
	ctx := context.Background()
	listKey := digestListKey(key)
	claimKey := digestClaimKey(key)

	events := []json.RawMessage{}
	entry, err := b.digest.Get(ctx, listKey)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(entry.Value(), &events); uerr != nil {
			return nil, fmt.Errorf("eventbus: digest unmarshal %q: %w", listKey, uerr)
		}
	case errors.Is(err, jetstream.ErrKeyNotFound):
		// Nothing accumulated.
	default:
		return nil, fmt.Errorf("eventbus: digest get %q: %w", listKey, err)
	}

	if err := b.digest.Delete(ctx, listKey); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, fmt.Errorf("eventbus: digest delete %q: %w", listKey, err)
	}
	if err := b.digest.Delete(ctx, claimKey); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, fmt.Errorf("eventbus: digest delete %q: %w", claimKey, err)
	}
	return events, nil
}
