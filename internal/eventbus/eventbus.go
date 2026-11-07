// Package eventbus owns notify's NATS client connection to the dagnats
// sidecar and notify's own private JetStream resources. The raw NATS
// connection and JetStream context are never exposed; callers operate
// through typed methods only.
package eventbus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	throttleBucket = "notify_throttle"
	digestBucket   = "notify_digest"

	// digestClaimBucket holds digest-window claims, separate from digestBucket
	// so a bucket-wide TTL can expire orphaned claims WITHOUT putting any lifetime
	// on the accumulating event lists (which stay in digestBucket and must never
	// expire mid-window). NATS KV TTL is bucket-wide, not per-key, so the two
	// concerns cannot share a bucket -- see maxDigestClaimTTL.
	digestClaimBucket  = "notify_digest_claims"
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

	// maxDigestClaimTTL bounds how long a digest-window claim survives before it
	// self-expires. A claim is created when a window's first event arrives and
	// released only when the window drains; nothing refreshes it in between, so
	// this must exceed the longest window a claim legitimately spans.
	//
	// Sizing is a deliberate tradeoff, not a hang backstop. Digest windows are in
	// practice daily/weekly/monthly; 45 days covers a monthly window with two
	// weeks of grace. The two failure directions are asymmetric and both
	// self-correct, so erring is not catastrophic either way:
	//   - a window LONGER than this TTL lets the claim expire mid-window, so the
	//     next event re-claims and a second windowed run fires -> a DUPLICATE
	//     digest for that window (annoying, transient, gone next window). Never
	//     event loss -- the event lists carry no TTL.
	//   - an ORPHANED claim (crash between claim and drain) self-heals within this
	//     TTL instead of locking the subscriber's digests forever, which is the
	//     bug this constant exists to fix.
	// A pathological config (e.g. an annual cron digest) would want a longer value
	// or a claim heartbeat; neither is worth the complexity at current usage.
	maxDigestClaimTTL = 45 * 24 * time.Hour

	// budgetBucket holds the cost-weighted fixed-window ingest counters that
	// BudgetAllow reads and writes (see budget.go).
	budgetBucket = "notify_ingest_budget"

	// maxBudgetBucketTTL is the bucket-wide eviction ceiling on
	// notify_ingest_budget. dagnats/NATS KV TTL is bucket-wide (a MaxAge on the
	// backing stream), NOT per-key, so BudgetAllow bakes the window epoch into
	// the key (like ThrottleAllow) and relies on this ceiling only to evict
	// dead past-window keys. It MUST exceed the longest window BudgetAllow is
	// ever called with (feedingest's 1h byte-budget window) by a safe margin —
	// otherwise a live long-window counter could age out mid-window and reset
	// to zero, undercounting and leaking the retention guard. 2h is >=2x the
	// longest current window; a tight ceiling also bounds stale-key growth
	// (short windows would otherwise strand tens of thousands of dead keys per
	// source before eviction).
	maxBudgetBucketTTL = 2 * time.Hour
)

// Bus is notify's handle on the event bus. It holds the client connection
// to the dagnats sidecar plus notify's own JetStream resources.
type Bus struct {
	nc *nats.Conn
	// ownsConn is set when Connect dialed nc; a connection passed to FromConn
	// belongs to the caller and Close leaves it open.
	ownsConn    bool
	js          jetstream.JetStream
	throttle    jetstream.KeyValue
	digest      jetstream.KeyValue
	digestClaim jetstream.KeyValue
	budget      jetstream.KeyValue
	// outputPrefix roots the dagnats step-output subjects the bus listens
	// on: "stream." for a standalone worker, "stream.p.<program>." for a
	// program's.
	outputPrefix string
}

// Option configures a Bus at Connect time.
type Option func(*busConfig)

// busConfig holds Connect-time tunables resolved from Options.
type busConfig struct {
	digestClaimTTL time.Duration
	hostResources  bool
	outputPrefix   string
}

// WithDigestClaimTTL overrides the digest-claim bucket TTL. Tests use a short
// value to exercise orphan-expiry without waiting a production window; leave it
// unset in production to get maxDigestClaimTTL.
func WithDigestClaimTTL(d time.Duration) Option {
	return func(c *busConfig) { c.digestClaimTTL = d }
}

// Connect dials natsURL, waits for the connection to be established, and
// ensures notify's JetStream resources exist (idempotent): the KV buckets
// Buckets declares and the streams Streams declares. The returned Bus owns the
// connection; the caller must Close it.
func Connect(ctx context.Context, natsURL string, opts ...Option) (*Bus, error) {
	nc, err := DialNATS(natsURL, "notify-eventbus", "dagnats sidecar (-nats-url)")
	if err != nil {
		return nil, err
	}
	b, err := FromConn(ctx, nc, opts...)
	if err != nil {
		nc.Close()
		return nil, err
	}
	b.ownsConn = true
	return b, nil
}

// FromConn builds a Bus on an existing connection and ensures the same
// JetStream resources Connect does. The connection stays the caller's: Close
// does not close it.
func FromConn(ctx context.Context, nc *nats.Conn, opts ...Option) (*Bus, error) {
	cfg := busConfig{digestClaimTTL: maxDigestClaimTTL, outputPrefix: "stream."}
	for _, opt := range opts {
		opt(&cfg)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("eventbus: jetstream init: %w", err)
	}

	// Claims live in their own bucket so their bucket-wide TTL expires orphaned
	// claims without ever touching the accumulating event lists.
	kv, err := ensureBuckets(ctx, js, cfg.hostResources, cfg.digestClaimTTL)
	if err != nil {
		return nil, err
	}
	if err := ensureStreams(ctx, js, cfg.hostResources); err != nil {
		return nil, err
	}

	return &Bus{
		nc: nc, js: js, throttle: kv[throttleBucket], digest: kv[digestBucket],
		digestClaim: kv[digestClaimBucket], budget: kv[budgetBucket], outputPrefix: cfg.outputPrefix,
	}, nil
}

// Close drains and closes the underlying NATS connection.
func (b *Bus) Close() error {
	if b == nil || b.nc == nil || !b.ownsConn {
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

// DeliveryEventKey is the stable identity of one delivery-status event: the
// message id and status of a domain.Message payload ("<id>:<status>"), or a
// SHA-256 of the payload when it carries no message id. PublishDelivery sets it
// as Nats-Msg-Id so JetStream drops duplicate publishes, and the analytics
// projector keys its rows on it so a redelivery counts once.
func DeliveryEventKey(data []byte) string {
	var ev struct{ ID, Status string }
	if json.Unmarshal(data, &ev) == nil && ev.ID != "" {
		return ev.ID + ":" + ev.Status
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PublishDelivery publishes a delivery-status event for an environment onto
// the durable NOTIFY_DELIVERY stream (JetStream, at-least-once) so it
// survives restarts and consumer downtime. Core-NATS subscribers on the same
// subject (SubscribeDelivery, SubscribeAllDelivery) still receive it live —
// JetStream publish is a normal NATS publish that the stream also captures.
// The event carries Nats-Msg-Id = DeliveryEventKey(data), so a duplicate
// publish within the stream's duplicate window is dropped.
func (b *Bus) PublishDelivery(environmentID string, data []byte) error {
	if environmentID == "" {
		return fmt.Errorf("eventbus: PublishDelivery: environmentID must be non-empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.js.Publish(ctx, deliverySubject(environmentID), data,
		jetstream.WithMsgID(DeliveryEventKey(data))); err != nil {
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

// DurableOption tweaks the config a ConsumeAllDeliveryDurable consumer is
// created with.
type DurableOption func(*jetstream.ConsumerConfig)

// FromNewMessages makes a durable that does not exist yet start at the stream's
// tail, skipping the retained backlog. It has no effect on an existing durable,
// which resumes from its acked position.
func FromNewMessages() DurableOption {
	return func(c *jetstream.ConsumerConfig) { c.DeliverPolicy = jetstream.DeliverNewPolicy }
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
//
// By default a durable that does not exist yet starts at the beginning of the
// stream (DeliverAll), replaying the retained backlog. FromNewMessages starts
// it at the stream's tail instead.
func (b *Bus) ConsumeAllDeliveryDurable(ctx context.Context, durable string, handler func(data []byte) error, opts ...DurableOption) (stop func() error, err error) {
	if durable == "" {
		return nil, fmt.Errorf("eventbus: ConsumeAllDeliveryDurable: durable must be non-empty")
	}

	cfg := jetstream.ConsumerConfig{
		Durable:        durable,
		FilterSubjects: []string{deliverySubjectAll},
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        30 * time.Second,
		MaxDeliver:     5,
	}
	for _, o := range opts {
		o(&cfg)
	}

	var cons jetstream.Consumer
	if cfg.DeliverPolicy != jetstream.DeliverAllPolicy {
		// DeliverPolicy only applies at creation and JetStream rejects an
		// update that changes it, so an existing durable is used as is.
		cons, err = b.js.Consumer(ctx, deliveryStream, durable)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			cons, err = b.js.CreateConsumer(ctx, deliveryStream, cfg)
		}
	} else {
		cons, err = b.js.CreateOrUpdateConsumer(ctx, deliveryStream, cfg)
	}
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
// builder's agent-loop step publishes its live Events on: stream.<runID>.build,
// or stream.p.<program>.<runID>.build from a program's worker. "build" is the
// builder's single agent-loop step id (aibuilder.AgentLoopStepID); the dagnats
// streaming convention is stream.<runID>.<stepID>.
func (b *Bus) buildStreamSubject(runID string) string {
	return b.outputPrefix + runID + ".build"
}

// SubscribeBuildStream delivers each AI-builder streaming Event for the run to
// handler until the returned unsubscribe func is called. Uses core NATS, so the
// subscription is ephemeral (no replay) — the console subscribes for the life of
// one generate request and drops it when the request ends.
func (b *Bus) SubscribeBuildStream(runID string, handler func([]byte)) (unsubscribe func() error, err error) {
	if runID == "" {
		return nil, fmt.Errorf("eventbus: SubscribeBuildStream: runID must be non-empty")
	}
	sub, err := b.nc.Subscribe(b.buildStreamSubject(runID), func(m *nats.Msg) {
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
// `window`, incrementing the fixed-window counter. The increment is a
// revision-CAS on the KV entry (see casSpend), so N concurrent callers
// serialise on the key and exactly `limit` are admitted per window (#321).
//
// An unresolved outcome (KV error, CAS retries exhausted) returns
// (false, err); the throttle step treats that as allow, because for an
// alerting system over-delivering under a sick broker beats swallowing a
// page. That is the caller's policy, not this function's.
func (b *Bus) ThrottleAllow(key string, limit int, window time.Duration) (allowed bool, err error) {
	if window <= 0 {
		return false, fmt.Errorf("eventbus: ThrottleAllow: window must be positive, got %s", window)
	}
	bucketKey := sanitizeKey(key) + "." +
		strconv.FormatInt(time.Now().UnixNano()/int64(window), 10)
	return casSpend(context.Background(), b.throttle, "throttle", bucketKey, 1, int64(limit))
}

// digestListKey returns the KV key holding the accumulated event list for key.
func digestListKey(key string) string { return sanitizeKey(key) + ".events" }

// digestClaimKey returns the KV key used to atomically claim the digest window.
func digestClaimKey(key string) string { return sanitizeKey(key) + ".claim" }

// maxDigestAppendAttempts bounds the optimistic-concurrency retry loops on the
// digest list (append and window close). Contention is per digest key, so a
// handful of simultaneous writers is the realistic worst case.
const maxDigestAppendAttempts = 64

// DigestAppend appends one event payload to the accumulator list at key. The
// read-modify-write is guarded by the KV revision (Create when absent, Update
// otherwise) and retried on conflict, so concurrent appends never lose events.
func (b *Bus) DigestAppend(key string, event json.RawMessage) error {
	ctx := context.Background()
	listKey := digestListKey(key)

	for range maxDigestAppendAttempts {
		var events []json.RawMessage
		var rev uint64
		entry, err := b.digest.Get(ctx, listKey)
		switch {
		case err == nil:
			rev = entry.Revision()
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
		if rev == 0 {
			_, err = b.digest.Create(ctx, listKey, data)
		} else {
			_, err = b.digest.Update(ctx, listKey, data, rev)
		}
		if err == nil {
			return nil
		}
		if isKVConflict(err) {
			continue
		}
		return fmt.Errorf("eventbus: digest put %q: %w", listKey, err)
	}
	return fmt.Errorf("eventbus: digest append %q: too much contention", listKey)
}

// isKVConflict reports whether err is an optimistic-concurrency loss (the key
// changed or appeared since it was read).
func isKVConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, jetstream.ErrKeyExists) {
		return true
	}
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

// digestSnapshotKey returns the key holding the closed window's batch for one
// workflow run.
func digestSnapshotKey(key, runID string) string {
	return sanitizeKey(key) + ".run." + sanitizeKey(runID)
}

// DigestClaim atomically claims the digest window for key: returns isOwner=true
// for the FIRST caller (which should start the windowed run), false for
// subsequent callers within the window. Uses KV Create (atomic) on a claim key.
func (b *Bus) DigestClaim(key string) (isOwner bool, err error) {
	ctx := context.Background()
	claimKey := digestClaimKey(key)

	// Claims live in their own TTL'd bucket, so an orphaned claim (a crash
	// between here and DigestDrain) expires on its own rather than blocking this
	// key's digests forever. See maxDigestClaimTTL.
	if _, err := b.digestClaim.Create(ctx, claimKey, []byte("1")); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}
		return false, fmt.Errorf("eventbus: digest claim %q: %w", claimKey, err)
	}
	return true, nil
}

// DigestBatch closes the digest window for key ONCE per workflow run and
// returns its events; every call with the same (key, runID) returns the same
// batch. The first call snapshots the live list into a run-scoped key, then
// clears the live list and the claim, so triggers arriving afterwards open the
// next window. Later channel steps of the run, and retries of any step, read
// the snapshot and never touch the live list.
//
// The snapshot lives in the digest-claim bucket, whose bucket-wide TTL
// (maxDigestClaimTTL) reclaims it; nothing deletes it explicitly because a
// retry may legitimately need it until the run ends.
func (b *Bus) DigestBatch(key, runID string) ([]json.RawMessage, error) {
	ctx := context.Background()
	listKey := digestListKey(key)
	snapKey := digestSnapshotKey(key, runID)

	if entry, err := b.digestClaim.Get(ctx, snapKey); err == nil {
		return decodeDigestList(snapKey, entry.Value())
	} else if !errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, fmt.Errorf("eventbus: digest get %q: %w", snapKey, err)
	}

	for range maxDigestAppendAttempts {
		events := []json.RawMessage{}
		var rev uint64
		entry, err := b.digest.Get(ctx, listKey)
		switch {
		case err == nil:
			rev = entry.Revision()
			if events, err = decodeDigestList(listKey, entry.Value()); err != nil {
				return nil, err
			}
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Nothing accumulated.
		default:
			return nil, fmt.Errorf("eventbus: digest get %q: %w", listKey, err)
		}

		data, err := json.Marshal(events)
		if err != nil {
			return nil, fmt.Errorf("eventbus: digest marshal %q: %w", snapKey, err)
		}
		// Snapshot first, then clear: a crash in between leaves the events in the
		// snapshot (a retry still sees them) rather than dropping them.
		if _, err := b.digestClaim.Put(ctx, snapKey, data); err != nil {
			return nil, fmt.Errorf("eventbus: digest put %q: %w", snapKey, err)
		}
		if rev != 0 {
			err := b.digest.Delete(ctx, listKey, jetstream.LastRevision(rev))
			if isKVConflict(err) {
				continue // an append landed after our read; re-snapshot it too
			}
			if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil, fmt.Errorf("eventbus: digest delete %q: %w", listKey, err)
			}
		}
		if err := b.digestClaim.Delete(ctx, digestClaimKey(key)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, fmt.Errorf("eventbus: digest delete %q: %w", digestClaimKey(key), err)
		}
		return events, nil
	}
	return nil, fmt.Errorf("eventbus: digest close %q: too much contention", listKey)
}

func decodeDigestList(key string, raw []byte) ([]json.RawMessage, error) {
	events := []json.RawMessage{}
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("eventbus: digest unmarshal %q: %w", key, err)
	}
	return events, nil
}
