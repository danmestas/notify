package eventbus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/domain"
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
)

// ensureFeedStream idempotently creates (or updates) the NOTIFY_FEED stream.
// Called from Connect, mirroring the NOTIFY_ANALYTICS/NOTIFY_DELIVERY blocks.
func ensureFeedStream(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     feedStream,
		Subjects: []string{feedSubjectAll},
		MaxAge:   feedStreamMaxAge,
		MaxBytes: feedStreamMaxBytes,
		Discard:  jetstream.DiscardOld,
		Storage:  jetstream.FileStorage,
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
