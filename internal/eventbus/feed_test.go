package eventbus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestValidateFeedTopic table-tests the subject-injection gate. No sidecar
// needed: this is pure validation logic.
func TestValidateFeedTopic(t *testing.T) {
	cases := []struct {
		name    string
		topic   string
		wantErr bool
	}{
		{"simple", "orders", false},
		{"multi-segment", "orders.eu-west-1.created", false},
		{"underscore-dash-digit", "a_b-c.d2", false},
		{"empty", "", true},
		{"leading-dot", ".orders", true},
		{"trailing-dot", "orders.", true},
		{"double-dot", "a..b", true},
		{"star-wildcard", "orders.*", true},
		{"bare-gt", ">", true},
		{"gt-wildcard-suffix", "orders.>", true},
		{"internal-whitespace", "or ders", true},
		{"dollar-sign", "orders.$JS.api", true},
		{"too-long", strings.Repeat("a", 129), true},
		{"exactly-max-length", strings.Repeat("a", 128), false},
		{"star-alone", "*", true},
		{"unicode", "orders.☺", true},
		{"nul-byte", "orders.\x00", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFeedTopic(tc.topic)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateFeedTopic(%q) error = %v, wantErr %v", tc.topic, err, tc.wantErr)
			}
		})
	}
}

// TestConnect_EnsuresFeedStream asserts Connect idempotently creates the
// NOTIFY_FEED stream with the expected subjects, retention, and discard
// policy, verified from a raw JetStream client independent of the Bus's
// private handles.
func TestConnect_EnsuresFeedStream(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus1, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	defer bus1.Close()

	bus2, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("second Connect (idempotent): %v", err)
	}
	defer bus2.Close()

	js := inspectJS(t, natsURL)
	stream, err := js.Stream(ctx, feedStream)
	if err != nil {
		t.Fatalf("Stream(%q): %v", feedStream, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if !slices.Contains(info.Config.Subjects, feedSubjectAll) {
		t.Fatalf("stream %q subjects = %v, want to contain %q",
			feedStream, info.Config.Subjects, feedSubjectAll)
	}
	if info.Config.MaxAge != feedStreamMaxAge {
		t.Fatalf("stream %q MaxAge = %v, want %v", feedStream, info.Config.MaxAge, feedStreamMaxAge)
	}
	if info.Config.MaxBytes != feedStreamMaxBytes {
		t.Fatalf("stream %q MaxBytes = %v, want %v", feedStream, info.Config.MaxBytes, feedStreamMaxBytes)
	}
}

func connectFeedBus(t *testing.T) *Bus {
	t.Helper()
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

// TestPublishFeed_RoundTrip publishes three events on env1/orders.created and
// asserts ConsumeFeed from seq 1 receives all three in order with strictly
// increasing Seq.
func TestPublishFeed_RoundTrip(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for i := range 3 {
		if err := bus.PublishFeed("env1", "orders.created", []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatalf("PublishFeed(%d): %v", i, err)
		}
	}

	got := make(chan FeedMsg, 10)
	stop, err := bus.ConsumeFeed(ctx, "env1", "orders.created", 1, time.Time{}, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed: %v", err)
	}
	defer func() { _ = stop() }()

	var msgs []FeedMsg
	deadline := time.After(5 * time.Second)
	for len(msgs) < 3 {
		select {
		case m := <-got:
			msgs = append(msgs, m)
		case <-deadline:
			t.Fatalf("timed out; only received %d messages: %v", len(msgs), msgs)
		}
	}
	for i, m := range msgs {
		want := fmt.Sprintf(`{"n":%d}`, i)
		if string(m.Data) != want {
			t.Fatalf("msgs[%d].Data = %q, want %q", i, m.Data, want)
		}
		if i > 0 && msgs[i].Seq <= msgs[i-1].Seq {
			t.Fatalf("Seq not strictly increasing: %v", msgs)
		}
	}
}

// TestConsumeFeed_ResumeFromSeq publishes e1..e5 and asserts consuming from
// the stream sequence of e3 receives exactly e3,e4,e5.
func TestConsumeFeed_ResumeFromSeq(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	seqsCh := make(chan uint64, 10)
	collectStop, err := bus.ConsumeFeed(ctx, "env1", "orders.created", 0, time.Time{}, func(m FeedMsg) {
		seqsCh <- m.Seq
	})
	if err != nil {
		t.Fatalf("ConsumeFeed (collector): %v", err)
	}

	for i := range 5 {
		if err := bus.PublishFeed("env1", "orders.created", []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatalf("PublishFeed(%d): %v", i, err)
		}
	}

	var seqs []uint64
	deadline := time.After(5 * time.Second)
	for len(seqs) < 5 {
		select {
		case seq := <-seqsCh:
			seqs = append(seqs, seq)
		case <-deadline:
			t.Fatalf("timed out collecting seqs; got %v", seqs)
		}
	}
	_ = collectStop()
	fromSeq := seqs[2] // seq of e3

	got := make(chan FeedMsg, 10)
	stop, err := bus.ConsumeFeed(ctx, "env1", "orders.created", fromSeq, time.Time{}, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed (resume): %v", err)
	}
	defer func() { _ = stop() }()

	var msgs []FeedMsg
	deadline = time.After(5 * time.Second)
	for len(msgs) < 3 {
		select {
		case m := <-got:
			msgs = append(msgs, m)
		case <-deadline:
			t.Fatalf("timed out; only received %d messages: %v", len(msgs), msgs)
		}
	}
	want := []string{`{"n":2}`, `{"n":3}`, `{"n":4}`}
	for i, m := range msgs {
		if string(m.Data) != want[i] {
			t.Fatalf("msgs[%d].Data = %q, want %q", i, m.Data, want[i])
		}
	}
	select {
	case m := <-got:
		t.Fatalf("unexpected extra message: %v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestConsumeFeed_BacklogThenLive proves the backlog-then-live handoff the
// SSE bridge depends on: events published before the consumer starts are
// delivered first, then events published while connected also arrive.
func TestConsumeFeed_BacklogThenLive(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := bus.PublishFeed("env1", "orders.created", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("PublishFeed(1): %v", err)
	}
	if err := bus.PublishFeed("env1", "orders.created", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("PublishFeed(2): %v", err)
	}

	got := make(chan FeedMsg, 10)
	stop, err := bus.ConsumeFeed(ctx, "env1", "orders.created", 1, time.Time{}, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed: %v", err)
	}
	defer func() { _ = stop() }()

	for _, want := range []string{`{"n":1}`, `{"n":2}`} {
		select {
		case m := <-got:
			if string(m.Data) != want {
				t.Fatalf("Data = %q, want %q", m.Data, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for backlog message %q", want)
		}
	}

	if err := bus.PublishFeed("env1", "orders.created", []byte(`{"n":3}`)); err != nil {
		t.Fatalf("PublishFeed(3): %v", err)
	}
	select {
	case m := <-got:
		if string(m.Data) != `{"n":3}` {
			t.Fatalf("Data = %q, want %q", m.Data, `{"n":3}`)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

// TestConsumeFeed_TenantIsolation proves a consumer scoped to one
// (environment, topic) never receives another environment's or another
// topic's events.
func TestConsumeFeed_TenantIsolation(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	gotA := make(chan FeedMsg, 10)
	stopA, err := bus.ConsumeFeed(ctx, "envA", "t", 0, time.Time{}, func(m FeedMsg) { gotA <- m })
	if err != nil {
		t.Fatalf("ConsumeFeed(envA/t): %v", err)
	}
	defer func() { _ = stopA() }()

	gotAT1 := make(chan FeedMsg, 10)
	stopAT1, err := bus.ConsumeFeed(ctx, "envA", "t1", 0, time.Time{}, func(m FeedMsg) { gotAT1 <- m })
	if err != nil {
		t.Fatalf("ConsumeFeed(envA/t1): %v", err)
	}
	defer func() { _ = stopAT1() }()

	if err := bus.PublishFeed("envB", "t", []byte(`{"who":"B-t"}`)); err != nil {
		t.Fatalf("PublishFeed(envB/t): %v", err)
	}
	if err := bus.PublishFeed("envA", "t2", []byte(`{"who":"A-t2"}`)); err != nil {
		t.Fatalf("PublishFeed(envA/t2): %v", err)
	}
	if err := bus.PublishFeed("envA", "t", []byte(`{"who":"A-t"}`)); err != nil {
		t.Fatalf("PublishFeed(envA/t): %v", err)
	}
	if err := bus.PublishFeed("envA", "t1", []byte(`{"who":"A-t1"}`)); err != nil {
		t.Fatalf("PublishFeed(envA/t1): %v", err)
	}

	select {
	case m := <-gotA:
		if string(m.Data) != `{"who":"A-t"}` {
			t.Fatalf("envA/t consumer received wrong data: %q", m.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for envA/t event")
	}
	select {
	case m := <-gotAT1:
		if string(m.Data) != `{"who":"A-t1"}` {
			t.Fatalf("envA/t1 consumer received wrong data: %q", m.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for envA/t1 event")
	}

	// Neither consumer should receive anything else.
	select {
	case m := <-gotA:
		t.Fatalf("envA/t consumer received unexpected extra message: %v", m)
	case <-time.After(500 * time.Millisecond):
	}
	select {
	case m := <-gotAT1:
		t.Fatalf("envA/t1 consumer received unexpected extra message: %v", m)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestPublishFeed_RejectsInvalidTopic asserts wildcard/injection attempts
// error before touching NATS.
func TestPublishFeed_RejectsInvalidTopic(t *testing.T) {
	bus := connectFeedBus(t)

	for _, topic := range []string{"", "orders.*", ">", "a..b", "or ders"} {
		if err := bus.PublishFeed("env1", topic, []byte(`{}`)); err == nil {
			t.Fatalf("PublishFeed(env1, %q, ...) = nil error, want error", topic)
		}
	}
}

// TestConsumeFeed_RejectsInvalidTopic asserts wildcard/injection attempts
// error before touching NATS, and an empty environmentID is rejected too.
func TestConsumeFeed_RejectsInvalidTopic(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, topic := range []string{"", "orders.*", ">", "a..b", "or ders"} {
		if _, err := bus.ConsumeFeed(ctx, "env1", topic, 0, time.Time{}, func(FeedMsg) {}); err == nil {
			t.Fatalf("ConsumeFeed(env1, %q, ...) = nil error, want error", topic)
		}
	}
	if _, err := bus.ConsumeFeed(ctx, "", "orders", 0, time.Time{}, func(FeedMsg) {}); err == nil {
		t.Fatal("ConsumeFeed(\"\", orders, ...) = nil error, want error")
	}
}

// TestFeed_RejectsInvalidEnvID asserts an environmentID that is empty or
// contains subject-injection characters (wildcards, dots — an envID is a
// single subject token, unlike a topic) is rejected by BOTH PublishFeed and
// ConsumeFeed before any NATS interaction. This backs the tenant-isolation
// promise: a crafted envID must never be concatenated raw into the subject.
func TestFeed_RejectsInvalidEnvID(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, envID := range []string{"", "*", ">", "a.b", "en v", "$SYS"} {
		if err := bus.PublishFeed(envID, "orders", []byte(`{}`)); err == nil {
			t.Fatalf("PublishFeed(%q, orders, ...) = nil error, want error", envID)
		}
		if _, err := bus.ConsumeFeed(ctx, envID, "orders", 0, time.Time{}, func(FeedMsg) {}); err == nil {
			t.Fatalf("ConsumeFeed(%q, orders, ...) = nil error, want error", envID)
		}
	}
}

// TestConsumeFeed_FromTime asserts a time-based start returns only events
// published after the timestamp.
func TestConsumeFeed_FromTime(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := bus.PublishFeed("env1", "orders.created", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("PublishFeed(1): %v", err)
	}
	// Give the stream a moment so the cutoff strictly separates e1 from e2.
	time.Sleep(1100 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(1100 * time.Millisecond)

	if err := bus.PublishFeed("env1", "orders.created", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("PublishFeed(2): %v", err)
	}

	got := make(chan FeedMsg, 10)
	stop, err := bus.ConsumeFeed(ctx, "env1", "orders.created", 0, cutoff, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed: %v", err)
	}
	defer func() { _ = stop() }()

	select {
	case m := <-got:
		if string(m.Data) != `{"n":2}` {
			t.Fatalf("Data = %q, want %q", m.Data, `{"n":2}`)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-cutoff message")
	}
	select {
	case m := <-got:
		t.Fatalf("received unexpected message before/around cutoff: %v", m)
	case <-time.After(500 * time.Millisecond):
	}
}
