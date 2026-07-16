package eventbus

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestPublishFeedBatch_OrderAndAcks publishes a 10-event batch and asserts
// ConsumeFeed from seq 1 receives all 10 in batch order with strictly
// increasing stream seqs, and PublishFeedBatch reports (10, nil) on the
// all-success path.
func TestPublishFeedBatch_OrderAndAcks(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	events := make([]FeedEvent, 10)
	for i := range events {
		events[i] = FeedEvent{Data: []byte(fmt.Sprintf(`{"n":%d}`, i))}
	}

	n, err := bus.PublishFeedBatch("env1", "agent.tokens", events)
	if err != nil {
		t.Fatalf("PublishFeedBatch: %v", err)
	}
	if n != 10 {
		t.Fatalf("PublishFeedBatch n = %d, want 10", n)
	}

	got := make(chan FeedMsg, 20)
	stop, err := bus.ConsumeFeed(ctx, "env1", "agent.tokens", 1, time.Time{}, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed: %v", err)
	}
	defer func() { _ = stop() }()

	var msgs []FeedMsg
	deadline := time.After(5 * time.Second)
	for len(msgs) < 10 {
		select {
		case m := <-got:
			msgs = append(msgs, m)
		case <-deadline:
			t.Fatalf("timed out; only received %d messages", len(msgs))
		}
	}
	for i, m := range msgs {
		want := fmt.Sprintf(`{"n":%d}`, i)
		if string(m.Data) != want {
			t.Fatalf("msgs[%d].Data = %q, want %q", i, m.Data, want)
		}
		if i > 0 && msgs[i].Seq <= msgs[i-1].Seq {
			t.Fatalf("Seq not strictly increasing at %d: %v", i, msgs)
		}
	}
}

// TestPublishFeedBatch_DedupByMsgID publishes a batch carrying MsgID "s1/e1",
// then re-publishes the same MsgID (a producer retry) alongside a fresh
// MsgID. The dedup window on NOTIFY_FEED absorbs the retry so the consumer
// sees the first event exactly once and the fresh event once.
func TestPublishFeedBatch_DedupByMsgID(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	got := make(chan FeedMsg, 20)
	stop, err := bus.ConsumeFeed(ctx, "env1", "agent.tokens", 1, time.Time{}, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed: %v", err)
	}
	defer func() { _ = stop() }()

	if n, err := bus.PublishFeedBatch("env1", "agent.tokens", []FeedEvent{
		{MsgID: "s1/e1", Data: []byte(`{"e":1}`)},
	}); err != nil || n != 1 {
		t.Fatalf("PublishFeedBatch(first) = (%d, %v), want (1, nil)", n, err)
	}

	// Retry the same MsgID (dup) plus a new MsgID. Both publishes ack; the
	// duplicate is absorbed server-side and never re-delivered.
	if n, err := bus.PublishFeedBatch("env1", "agent.tokens", []FeedEvent{
		{MsgID: "s1/e1", Data: []byte(`{"e":1}`)},
		{MsgID: "s1/e2", Data: []byte(`{"e":2}`)},
	}); err != nil || n != 2 {
		t.Fatalf("PublishFeedBatch(retry) = (%d, %v), want (2, nil)", n, err)
	}

	var datas []string
	deadline := time.After(3 * time.Second)
	for len(datas) < 2 {
		select {
		case m := <-got:
			datas = append(datas, string(m.Data))
		case <-deadline:
			t.Fatalf("timed out; received %v", datas)
		}
	}
	// No third message (the dup) should arrive.
	select {
	case m := <-got:
		t.Fatalf("unexpected extra (duplicate) message: %q", m.Data)
	case <-time.After(500 * time.Millisecond):
	}

	want := []string{`{"e":1}`, `{"e":2}`}
	for i, d := range datas {
		if d != want[i] {
			t.Fatalf("datas[%d] = %q, want %q", i, d, want[i])
		}
	}
}

// TestPublishFeedBatch_RejectsInvalidTopicAndEnv asserts wildcard/injection
// topics and dotted/empty envIDs error before any publish, returning (0, err).
func TestPublishFeedBatch_RejectsInvalidTopicAndEnv(t *testing.T) {
	bus := connectFeedBus(t)

	ev := []FeedEvent{{Data: []byte(`{}`)}}
	for _, topic := range []string{"", "orders.*", ">", "a..b", "or ders"} {
		if n, err := bus.PublishFeedBatch("env1", topic, ev); err == nil || n != 0 {
			t.Fatalf("PublishFeedBatch(env1, %q) = (%d, %v), want (0, err)", topic, n, err)
		}
	}
	for _, envID := range []string{"", "*", ">", "a.b", "en v", "$SYS"} {
		if n, err := bus.PublishFeedBatch(envID, "orders", ev); err == nil || n != 0 {
			t.Fatalf("PublishFeedBatch(%q, orders) = (%d, %v), want (0, err)", envID, n, err)
		}
	}
}

// TestPublishFeedBatch_EmptyBatch asserts an empty batch is a no-op that
// returns (0, nil) without publishing.
func TestPublishFeedBatch_EmptyBatch(t *testing.T) {
	bus := connectFeedBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	n, err := bus.PublishFeedBatch("env1", "agent.tokens", nil)
	if err != nil || n != 0 {
		t.Fatalf("PublishFeedBatch(nil) = (%d, %v), want (0, nil)", n, err)
	}

	got := make(chan FeedMsg, 4)
	stop, err := bus.ConsumeFeed(ctx, "env1", "agent.tokens", 0, time.Time{}, func(m FeedMsg) {
		got <- m
	})
	if err != nil {
		t.Fatalf("ConsumeFeed: %v", err)
	}
	defer func() { _ = stop() }()

	select {
	case m := <-got:
		t.Fatalf("empty batch published something: %q", m.Data)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestPublishFeedBatch_ConcurrentBatchesDoNotBlockEachOther runs two
// concurrent PublishFeedBatch calls on the SAME *Bus (same shared
// jetstream.JetStream context, exactly as S4's ingest endpoint will issue one
// call per concurrent HTTP request) and asserts both complete promptly with
// their own correct n, independent of each other. This guards the
// process-global-barrier regression: in nats.go, JetStream.PublishAsyncComplete()
// returns a channel that closes only once the ENTIRE shared context's
// async-ack pool (js.publisher.acks) is empty, across every caller — waiting
// on it (instead of on this batch's own PubAckFutures) would serialize
// unrelated concurrent batches behind each other.
//
// A wall-clock A-must-finish-strictly-before-B style assertion was tried and
// discarded: nats.go's async publisher stalls the sender once
// PublishAsyncPending reaches its default cap (4000) and unstalls again the
// moment a single ack drops it back under that cap, so the shared ack pool
// cycles through zero repeatedly over the course of a large batch rather than
// staying non-empty until the batch fully drains. That makes the shared
// completion channel close (and reopen) many times during a large batch's
// run, so timing alone does not reliably discriminate the bug on a fast local
// broker. Verified empirically against the unfixed implementation: even a
// small batch issued while a 30k-event batch had 1000+ acks outstanding
// still returned in ~34ms (vs. ~958ms for the large batch) — i.e. no
// reliable timing signal exists locally, even though the bug is real (the
// small batch's own future genuinely does have to wait behind whatever
// snapshot of the shared pool existed the instant it called
// PublishAsyncComplete(), it just isn't guaranteed to be a large one). Per
// the "at minimum a -race test with 2 concurrent batches that both succeed"
// guidance, this test instead asserts correctness and a generous bound to
// catch genuine deadlocks/cross-batch stalls.
func TestPublishFeedBatch_ConcurrentBatchesDoNotBlockEachOther(t *testing.T) {
	bus := connectFeedBus(t)

	const batchN = 200
	batchA := make([]FeedEvent, batchN)
	batchB := make([]FeedEvent, batchN)
	for i := 0; i < batchN; i++ {
		batchA[i] = FeedEvent{Data: []byte(fmt.Sprintf(`{"batch":"a","n":%d}`, i))}
		batchB[i] = FeedEvent{Data: []byte(fmt.Sprintf(`{"batch":"b","n":%d}`, i))}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	var gotA, gotB int
	var errA, errB error
	var durA, durB time.Duration

	go func() {
		defer wg.Done()
		<-start
		t0 := time.Now()
		gotA, errA = bus.PublishFeedBatch("env1", "concurrent-a", batchA)
		durA = time.Since(t0)
	}()

	go func() {
		defer wg.Done()
		<-start
		t0 := time.Now()
		gotB, errB = bus.PublishFeedBatch("env1", "concurrent-b", batchB)
		durB = time.Since(t0)
	}()

	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(feedBatchAckWait + 5*time.Second):
		t.Fatalf("two concurrent %d-event PublishFeedBatch calls did not both complete within %s "+
			"of the ack-wait bound; one batch appears to be blocked on the other's acks",
			batchN, feedBatchAckWait+5*time.Second)
	}

	if errA != nil {
		t.Fatalf("batch A PublishFeedBatch: %v", errA)
	}
	if gotA != batchN {
		t.Fatalf("batch A n = %d, want %d", gotA, batchN)
	}
	if errB != nil {
		t.Fatalf("batch B PublishFeedBatch: %v", errB)
	}
	if gotB != batchN {
		t.Fatalf("batch B n = %d, want %d", gotB, batchN)
	}

	// Each call should complete in well under the ack-wait bound on a healthy
	// local broker; a duration approaching feedBatchAckWait would indicate
	// one batch got stuck waiting on the other's acks rather than its own.
	if durA >= feedBatchAckWait || durB >= feedBatchAckWait {
		t.Fatalf("batch A took %v, batch B took %v; expected both well under the %s ack-wait bound",
			durA, durB, feedBatchAckWait)
	}
}

// TestConnect_FeedStreamHasDuplicateWindow asserts Connect configures the
// NOTIFY_FEED stream with the 2-minute Nats-Msg-Id dedup window (plus the
// existing MaxBytes/DiscardOld retention config remaining intact).
func TestConnect_FeedStreamHasDuplicateWindow(t *testing.T) {
	natsURL := startSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	js := inspectJS(t, natsURL)
	stream, err := js.Stream(ctx, feedStream)
	if err != nil {
		t.Fatalf("Stream(%q): %v", feedStream, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.Duplicates != feedStreamDuplicates {
		t.Fatalf("stream %q Duplicates = %v, want %v", feedStream, info.Config.Duplicates, feedStreamDuplicates)
	}
	if info.Config.MaxBytes != feedStreamMaxBytes {
		t.Fatalf("stream %q MaxBytes = %v, want %v", feedStream, info.Config.MaxBytes, feedStreamMaxBytes)
	}
}
