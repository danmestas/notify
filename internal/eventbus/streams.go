package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/danmestas/dagnats/program"
	"github.com/nats-io/nats.go/jetstream"
)

// WithHostResources says the host has already created notify's streams and KV
// buckets, as dagnats does for a program: registration provisions Streams()
// at the sizes in the program's Grant and Buckets() at their declared sizes. A
// program's NATS user may not create or update a stream, so FromConn then
// creates and reconfigures nothing: it binds what exists and fails naming any
// stream or bucket that is missing.
func WithHostResources() Option {
	return func(c *busConfig) { c.hostResources = true }
}

// WithProgramOutput says the tasks run on dagnats program name's worker,
// which publishes step output under stream.p.<name>. instead of stream., so
// the bus listens there.
func WithProgramOutput(name string) Option {
	if name == "" {
		panic("eventbus: WithProgramOutput: name must not be empty")
	}
	return func(c *busConfig) { c.outputPrefix = "stream.p." + name + "." }
}

// Streams declares notify's own JetStream streams for its program manifest.
// Subjects and ages match what FromConn creates on its own. Each size is a
// fraction of the dagnats store budget with a floor: NOTIFY_FEED holds tenant
// feed data, NOTIFY_DELIVERY a week of delivery events, and NOTIFY_ANALYTICS
// has no publisher today.
func Streams() []program.StreamSpec {
	return []program.StreamSpec{
		{
			Name: analyticsStream, Subjects: []string{analyticsSubject},
			MaxBytesFloor: 16 << 20,
		},
		{
			Name: deliveryStream, Subjects: []string{deliverySubjectAll},
			MaxBytesFraction: 0.01, MaxBytesFloor: 64 << 20, MaxAge: deliveryStreamMaxAge,
		},
		{
			Name: feedStream, Subjects: []string{feedSubjectAll},
			MaxBytesFraction: 0.02, MaxBytesFloor: 128 << 20, MaxAge: feedStreamMaxAge,
		},
	}
}

// Buckets declares notify's own KV buckets for its program manifest, and is
// what FromConn creates on its own. A bucket's MaxBytes is a hard cap (a full
// bucket refuses writes), so each is sized far above what its keys hold: the
// digest bucket keeps every pending window's events, the others hold small
// counters and claims that their TTL evicts. dagnats refuses a later manifest
// that lowers a TTL or History, or caps a bucket below what it holds.
func Buckets() []program.BucketSpec {
	return []program.BucketSpec{
		{Name: throttleBucket, History: 1, TTL: maxThrottleBucketTTL, MaxBytes: 64 << 20},
		{Name: digestBucket, History: 1, MaxBytes: 256 << 20},
		{Name: digestClaimBucket, History: 1, TTL: maxDigestClaimTTL, MaxBytes: 16 << 20},
		{Name: budgetBucket, History: 1, TTL: maxBudgetBucketTTL, MaxBytes: 32 << 20},
	}
}

// ensureStreams creates notify's streams or, when the host owns them, checks
// that each exists.
func ensureStreams(ctx context.Context, js jetstream.JetStream, hostResources bool) error {
	if hostResources {
		for _, s := range Streams() {
			if _, err := js.Stream(ctx, s.Name); err != nil {
				return fmt.Errorf("eventbus: stream %q not provisioned by the host: %w", s.Name, err)
			}
		}
		return nil
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     analyticsStream,
		Subjects: []string{analyticsSubject},
	}); err != nil {
		return fmt.Errorf("eventbus: ensure stream %q: %w", analyticsStream, err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     deliveryStream,
		Subjects: []string{deliverySubjectAll},
		MaxAge:   deliveryStreamMaxAge,
		Storage:  jetstream.FileStorage,
	}); err != nil {
		return fmt.Errorf("eventbus: ensure stream %q: %w", deliveryStream, err)
	}
	return ensureFeedStream(ctx, js)
}

// ensureBuckets creates notify's buckets or, when the host owns them, binds
// them, keyed by name. digestClaimTTL is the claim bucket's TTL; only a
// standalone bus can change it, since a host-provisioned bucket has the TTL
// Buckets declares.
func ensureBuckets(
	ctx context.Context, js jetstream.JetStream, hostResources bool, digestClaimTTL time.Duration,
) (map[string]jetstream.KeyValue, error) {
	if hostResources && digestClaimTTL != maxDigestClaimTTL {
		return nil, fmt.Errorf("eventbus: WithDigestClaimTTL cannot change a host-provisioned bucket")
	}
	out := make(map[string]jetstream.KeyValue, len(Buckets()))
	for _, b := range Buckets() {
		if hostResources {
			kv, err := js.KeyValue(ctx, b.Name)
			if err != nil {
				return nil, fmt.Errorf("eventbus: bucket %q not provisioned by the host: %w", b.Name, err)
			}
			out[b.Name] = kv
			continue
		}
		ttl := b.TTL
		if b.Name == digestClaimBucket {
			ttl = digestClaimTTL
		}
		kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket: b.Name, History: b.History, TTL: ttl, MaxBytes: b.MaxBytes,
		})
		if err != nil {
			return nil, fmt.Errorf("eventbus: ensure kv %q: %w", b.Name, err)
		}
		out[b.Name] = kv
	}
	return out, nil
}
