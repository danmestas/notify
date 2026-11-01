package eventbus

import (
	"context"
	"fmt"

	"github.com/danmestas/dagnats/program"
	"github.com/nats-io/nats.go/jetstream"
)

// WithHostStreams says the host has already created notify's streams, as
// dagnats does for a program: registration provisions Streams() at the sizes
// in the program's Grant. FromConn then creates and reconfigures no stream, so
// the granted sizes stand, and it fails naming any stream that is missing.
func WithHostStreams() Option {
	return func(c *busConfig) { c.hostStreams = true }
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

// ensureStreams creates notify's streams or, when the host owns them, checks
// that each exists.
func ensureStreams(ctx context.Context, js jetstream.JetStream, hostStreams bool) error {
	if hostStreams {
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
