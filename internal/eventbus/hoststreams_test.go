package eventbus

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/dagnats/program"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/notify/internal/dagtest"
)

// TestStreams_AreTheStreamsFromConnCreates pins that the declared specs and
// the standalone create path name the same streams and subjects.
func TestStreams_AreTheStreamsFromConnCreates(t *testing.T) {
	want := map[string]string{
		analyticsStream: analyticsSubject,
		deliveryStream:  deliverySubjectAll,
		feedStream:      feedSubjectAll,
	}
	specs := Streams()
	if len(specs) != len(want) {
		t.Fatalf("Streams() = %d specs, want %d", len(specs), len(want))
	}
	for _, s := range specs {
		if len(s.Subjects) != 1 || want[s.Name] != s.Subjects[0] {
			t.Errorf("stream %s subjects %v, want [%s]", s.Name, s.Subjects, want[s.Name])
		}
	}
}

// TestWithHostStreams_RefusesMissingAndKeepsProvisionedSize: in program mode
// the host creates notify's streams. FromConn must name a stream the host did
// not create, and must not resize one it did.
func TestWithHostStreams_RefusesMissingAndKeepsProvisionedSize(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	ctx := context.Background()

	_, err = FromConn(ctx, nc, WithHostStreams())
	if err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("FromConn before provisioning: err = %v, want a not-provisioned error", err)
	}

	m := program.Manifest{Name: "notify", Dagnats: ">=0.0.40", Streams: Streams()}
	g := program.Grant{Replicas: 1, StreamMaxBytes: map[string]int64{}}
	for _, s := range m.Streams {
		g.StreamMaxBytes[s.Name] = 8 << 20
		g.StoreBudgetBytes += 8 << 20
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	if err := program.Provision(ctx, js, m, g); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bus, err := FromConn(ctx, nc, WithHostStreams())
	if err != nil {
		t.Fatalf("FromConn after provisioning: %v", err)
	}
	defer func() { _ = bus.Close() }()
	for _, s := range m.Streams {
		stream, err := js.Stream(ctx, s.Name)
		if err != nil {
			t.Fatalf("stream %s: %v", s.Name, err)
		}
		if got := stream.CachedInfo().Config.MaxBytes; got != 8<<20 {
			t.Errorf("stream %s MaxBytes = %d after FromConn, want the provisioned %d", s.Name, got, 8<<20)
		}
	}
}
