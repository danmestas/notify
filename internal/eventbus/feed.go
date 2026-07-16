package eventbus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Names and retention for notify's stream-channel feed. This is the durable,
// replayable event feed the "stream" step type publishes to and the SSE
// bridge (feedapi) reads from.
const (
	feedStream     = "NOTIFY_FEED"
	feedSubjectAll = "notify.feed.>"

	// feedStreamMaxAge bounds how long feed events are retained. Clients
	// resuming via `from=` (seq or time) can only replay within this window.
	feedStreamMaxAge = 7 * 24 * time.Hour

	// feedStreamMaxBytes caps total retained feed data across ALL tenants and
	// topics so a chatty tenant can't grow the stream unbounded; combined
	// with DiscardOld the oldest events are evicted first once the cap is
	// hit, independent of feedStreamMaxAge.
	feedStreamMaxBytes = 1 << 30 // 1 GiB

	// maxFeedEnvIDLen bounds the environmentID validateEnvID accepts. envIDs
	// are system-generated single-token identifiers (ULIDs); this is a
	// generous ceiling, not a tight fit.
	maxFeedEnvIDLen = 128

	// feedStreamDuplicates is the JetStream Nats-Msg-Id dedup horizon on
	// NOTIFY_FEED. PublishFeedBatch stamps ingested events carrying a producer
	// id with a Nats-Msg-Id header; within this window the stream absorbs
	// re-publishes so a producer retrying a whole batch does not double-store
	// its acked events. 2 minutes covers realistic webhook retry schedules at
	// negligible memory cost.
	feedStreamDuplicates = 2 * time.Minute

	// feedBatchAckWait bounds how long PublishFeedBatch waits for the async
	// publishes it issued to be acked by the server before giving up and
	// reporting how many leading events were confirmed durable.
	feedBatchAckWait = 10 * time.Second
)

// ensureFeedStream idempotently creates (or updates) the NOTIFY_FEED stream.
// Called from Connect, mirroring the NOTIFY_ANALYTICS/NOTIFY_DELIVERY blocks.
func ensureFeedStream(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       feedStream,
		Subjects:   []string{feedSubjectAll},
		MaxAge:     feedStreamMaxAge,
		MaxBytes:   feedStreamMaxBytes,
		Discard:    jetstream.DiscardOld,
		Duplicates: feedStreamDuplicates,
		Storage:    jetstream.FileStorage,
	})
	if err != nil {
		return fmt.Errorf("eventbus: ensure stream %q: %w", feedStream, err)
	}
	return nil
}

// validateEnvID is the subject-injection gate for the environmentID segment
// of a feed subject. Unlike a topic, an environmentID is a SINGLE subject
// token — so dots are rejected too (a dotted envID would span multiple
// subject tokens and could match another environment's subtree). envIDs are
// system-generated ULIDs which pass cleanly; this backs the isolation
// promise ConsumeFeed makes as defense in depth.
func validateEnvID(environmentID string) error {
	if environmentID == "" {
		return fmt.Errorf("eventbus: environmentID must be non-empty")
	}
	if len(environmentID) > maxFeedEnvIDLen {
		return fmt.Errorf("eventbus: environmentID exceeds %d bytes", maxFeedEnvIDLen)
	}
	for _, r := range environmentID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			// allowed
		default:
			return fmt.Errorf("eventbus: environmentID %q contains disallowed character %q", environmentID, r)
		}
	}
	return nil
}

// feedSubject builds the JetStream subject for an environment's feed topic:
// notify.feed.<environmentID>.<topic>.
func feedSubject(environmentID, topic string) string {
	return "notify.feed." + environmentID + "." + topic
}

// PublishFeed publishes one stream-channel event durably to the
// environment's feed topic on NOTIFY_FEED. topic must already be rendered
// (concrete); it is re-validated here (via domain.ValidateFeedTopic) as
// defense in depth — the delivery package's HandleStream already hard-gates
// on the same rule before ever calling this.
func (b *Bus) PublishFeed(environmentID, topic string, data []byte) error {
	if err := validateEnvID(environmentID); err != nil {
		return fmt.Errorf("eventbus: PublishFeed: %w", err)
	}
	if err := domain.ValidateFeedTopic(topic); err != nil {
		return fmt.Errorf("eventbus: PublishFeed: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.js.Publish(ctx, feedSubject(environmentID, topic), data); err != nil {
		return fmt.Errorf("eventbus: PublishFeed: %w", err)
	}
	return nil
}

// FeedEvent is one pre-serialized ingest event destined for an environment's
// feed topic. MsgID, when non-empty, is set as the JetStream Nats-Msg-Id
// header so the stream's dedup window (feedStreamDuplicates) absorbs producer
// retries; an empty MsgID publishes with no dedup id.
type FeedEvent struct {
	MsgID string
	Data  []byte
}

// PublishFeedBatch publishes events to the environment's feed topic on
// NOTIFY_FEED using async publishes with a single bounded completion wait,
// preserving order within the batch. envID and topic are validated exactly as
// PublishFeed (both must pass before ANY publish is issued); an empty batch is
// a no-op returning (0, nil).
//
// It returns n = the number of LEADING events confirmed durably acked — the
// largest k such that every future for events[:k] returned Ok() before the
// first future that returned Err() (futures collected in batch order, counting
// stops at the first error) — together with that first error.
//
// On a non-nil error the caller MUST retry the WHOLE batch. That retry is
// idempotent for events carrying a MsgID (the JetStream dedup window absorbs
// the re-publishes) and merely duplicative for events minted without a
// producer id (accepted per plan §0.5). Events at or after index n MAY have
// partially landed: under async fan-out an event after the first failure can
// still have been durably acked, so callers must NOT assume events[n:] were
// dropped. On full success it returns (len(events), nil).
func (b *Bus) PublishFeedBatch(environmentID, topic string, events []FeedEvent) (n int, err error) {
	if verr := validateEnvID(environmentID); verr != nil {
		return 0, fmt.Errorf("eventbus: PublishFeedBatch: %w", verr)
	}
	if verr := domain.ValidateFeedTopic(topic); verr != nil {
		return 0, fmt.Errorf("eventbus: PublishFeedBatch: %w", verr)
	}
	if len(events) == 0 {
		return 0, nil
	}

	subject := feedSubject(environmentID, topic)
	futures := make([]jetstream.PubAckFuture, 0, len(events))
	var publishErr error
	for _, e := range events {
		msg := &nats.Msg{Subject: subject, Data: e.Data}
		if e.MsgID != "" {
			msg.Header = nats.Header{nats.MsgIdHdr: []string{e.MsgID}}
		}
		f, perr := b.js.PublishMsgAsync(msg)
		if perr != nil {
			// The async buffer rejected this enqueue; stop issuing further
			// publishes and count only the leading acks among what we sent.
			publishErr = fmt.Errorf("eventbus: PublishFeedBatch: publish event %d: %w", len(futures), perr)
			break
		}
		futures = append(futures, f)
	}

	// Wait on THIS batch's own futures, not JetStream's PublishAsyncComplete():
	// b.js is a single jetstream.JetStream context shared by every caller
	// (e.g. every concurrent ingest request), and PublishAsyncComplete()'s
	// channel closes only once that ENTIRE shared context's async-ack pool is
	// empty — waiting on it would block this batch on unrelated concurrent
	// batches' acks. A single bounded deadline covers the whole wait loop so
	// a stalled broker cannot hang the caller.
	ctx, cancel := context.WithTimeout(context.Background(), feedBatchAckWait)
	defer cancel()
	for _, f := range futures {
		select {
		case <-f.Ok():
			n++
		case ackErr := <-f.Err():
			return n, fmt.Errorf("eventbus: PublishFeedBatch: ack event %d: %w", n, ackErr)
		case <-ctx.Done():
			return n, fmt.Errorf("eventbus: PublishFeedBatch: event %d ack pending after %s: %w", n, feedBatchAckWait, ctx.Err())
		}
	}

	if publishErr != nil {
		return n, publishErr
	}
	return n, nil
}

// FeedMsg is one durable feed event with its stream sequence (the SSE id).
type FeedMsg struct {
	Seq  uint64
	Data []byte
}

// ConsumeFeed streams the environment's feed topic to handler via a
// JetStream ordered (ephemeral, auto-healing) consumer filtered server-side
// to the (environmentID, topic) subject — tenant isolation lives here, not
// in the HTTP layer.
//
// fromSeq > 0 starts at that stream sequence, inclusive; else fromTime
// non-zero starts at that time; else only new messages (published after the
// consumer starts) are delivered. Resume math ("last seen id" -> fromSeq =
// lastSeen+1) belongs to the caller (the SSE bridge), not here.
//
// Ordered consumers do not ack; the client library handles gap detection and
// consumer re-creation transparently. The returned stop drains the consume
// loop.
func (b *Bus) ConsumeFeed(ctx context.Context, environmentID, topic string, fromSeq uint64, fromTime time.Time, handler func(FeedMsg)) (stop func() error, err error) {
	if err := validateEnvID(environmentID); err != nil {
		return nil, fmt.Errorf("eventbus: ConsumeFeed: %w", err)
	}
	if err := domain.ValidateFeedTopic(topic); err != nil {
		return nil, fmt.Errorf("eventbus: ConsumeFeed: %w", err)
	}

	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{feedSubject(environmentID, topic)},
	}
	switch {
	case fromSeq > 0:
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = fromSeq
	case !fromTime.IsZero():
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		cfg.OptStartTime = &fromTime
	default:
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	}

	cons, err := b.js.OrderedConsumer(ctx, feedStream, cfg)
	if err != nil {
		return nil, fmt.Errorf("eventbus: ConsumeFeed: ordered consumer: %w", err)
	}

	consumeCtx, err := cons.Consume(func(m jetstream.Msg) {
		md, err := m.Metadata()
		if err != nil {
			// A JetStream message with no metadata cannot yield a stream
			// sequence (the SSE id); drop it, but observably — a silent drop
			// would hide a real protocol fault.
			slog.Default().Error("eventbus: ConsumeFeed: message metadata unavailable, dropping",
				"stream", feedStream, "err", err)
			return
		}
		handler(FeedMsg{Seq: md.Sequence.Stream, Data: m.Data()})
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: ConsumeFeed: consume: %w", err)
	}

	return func() error {
		consumeCtx.Drain()
		return nil
	}, nil
}
