package eventbus

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestBuckets_AreTheBucketsFromConnCreates pins that the declared bucket specs
// are the ones the standalone path creates, under notify's own prefix.
func TestBuckets_AreTheBucketsFromConnCreates(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	ctx := context.Background()
	bus, err := FromConn(ctx, nc)
	if err != nil {
		t.Fatalf("FromConn: %v", err)
	}
	defer func() { _ = bus.Close() }()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	want := []string{throttleBucket, digestBucket, digestClaimBucket, budgetBucket}
	var got []string
	for _, b := range Buckets() {
		got = append(got, b.Name)
		if !strings.HasPrefix(b.Name, "notify_") {
			t.Errorf("bucket %s is not under notify_", b.Name)
		}
		kv, err := js.KeyValue(ctx, b.Name)
		if err != nil {
			t.Fatalf("bucket %s: %v", b.Name, err)
		}
		status, err := kv.Status(ctx)
		if err != nil {
			t.Fatalf("bucket %s status: %v", b.Name, err)
		}
		if status.TTL() != b.TTL || status.History() != int64(b.History) {
			t.Errorf("bucket %s created with ttl %v history %d, declared %v %d",
				b.Name, status.TTL(), status.History(), b.TTL, b.History)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Buckets() = %v, want %v", got, want)
	}
}

// TestWithHostResources_RefusesMissingAndKeepsProvisionedSize: in program mode
// the host creates notify's streams and buckets. FromConn must name one the
// host did not create, and must not resize one it did.
func TestWithHostResources_RefusesMissingAndKeepsProvisionedSize(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	ctx := context.Background()

	_, err = FromConn(ctx, nc, WithHostResources())
	if err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("FromConn before provisioning: err = %v, want a not-provisioned error", err)
	}

	m := program.Manifest{Name: "notify", Dagnats: ">=0.0.40", Streams: Streams(), Buckets: Buckets()}
	g := program.Grant{Replicas: 1, StreamMaxBytes: map[string]int64{}}
	for _, s := range m.Streams {
		g.StreamMaxBytes[s.Name] = 8 << 20
		g.StoreBudgetBytes += 8 << 20
	}
	for _, b := range m.Buckets {
		g.StoreBudgetBytes += b.MaxBytes
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	if err := program.Provision(ctx, js, m, g); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bus, err := FromConn(ctx, nc, WithHostResources())
	if err != nil {
		t.Fatalf("FromConn after provisioning: %v", err)
	}
	for _, b := range m.Buckets {
		stream, err := js.Stream(ctx, "KV_"+b.Name)
		if err != nil {
			t.Fatalf("bucket %s: %v", b.Name, err)
		}
		if got := stream.CachedInfo().Config.MaxBytes; got != b.MaxBytes {
			t.Errorf("bucket %s MaxBytes = %d after FromConn, want the provisioned %d", b.Name, got, b.MaxBytes)
		}
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

// TestWithProgramOutput_SubscribesNamespacedBuildStream: a program's worker
// publishes step output under stream.p.<program>., so the bus must listen
// there for the AI builder's live events.
func TestWithProgramOutput_SubscribesNamespacedBuildStream(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	bus, err := FromConn(context.Background(), nc, WithProgramOutput("notify"))
	if err != nil {
		t.Fatalf("FromConn: %v", err)
	}
	got := make(chan []byte, 1)
	unsubscribe, err := bus.SubscribeBuildStream("run1", func(b []byte) { got <- b })
	if err != nil {
		t.Fatalf("SubscribeBuildStream: %v", err)
	}
	defer func() { _ = unsubscribe() }()
	if err := nc.Publish("stream.p.notify.run1.build", []byte("event")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case b := <-got:
		if string(b) != "event" {
			t.Fatalf("got %q, want event", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no build event on stream.p.notify.run1.build")
	}
}
