package cards

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/notify/internal/dagtest"
)

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"agents.prompt.cc.dmestas.claude": "cc.dmestas.claude",
		"agents.prompt.pi":                "pi",
		"agents.other.x":                  "",
		"":                                "",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSource_WatchesBucketCreatedLate: the bucket does not exist when Run
// starts; once it does the source picks up existing and later writes, an
// update replaces the card, and a delete removes it.
func TestSource_WatchesBucketCreatedLate(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	src := New(js, nil)
	src.retryMin, src.retryMax = 20*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { src.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(100 * time.Millisecond) // a few failed attempts
	if _, ok := src.Card("a.b"); ok {
		t.Fatal("card before the bucket exists")
	}

	ctxb := context.Background()
	kv, err := js.CreateKeyValue(ctxb, jetstream.KeyValueConfig{Bucket: Bucket})
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"name":"A",  "x":1}`)
	if _, err := kv.Put(ctxb, "a.b", first); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first card", func() bool { c, ok := src.Card("a.b"); return ok && string(c.Raw) == string(first) })
	c, _ := src.Card("a.b")
	if c.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not set")
	}

	second := []byte(`{"name":"B"}`)
	if _, err := kv.Put(ctxb, "a.b", second); err != nil {
		t.Fatal(err)
	}
	eventually(t, "updated card", func() bool { c, _ := src.Card("a.b"); return string(c.Raw) == string(second) })

	if err := kv.Delete(ctxb, "a.b"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted card", func() bool { _, ok := src.Card("a.b"); return !ok })
}
