package mesh_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"

	"github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/dagtest"
)

// v03Agent registers a fake Synadia v0.3 agent: a micro service named
// "agents" with protocol_version "0.3" and a prompt endpoint in queue group
// "agents". Each request is answered with the chunks script returns, in
// order, then the empty terminator. It returns the requests seen.
func v03Agent(t *testing.T, nc *nats.Conn, subject string, script func(req micro.Request) [][]byte) func() []micro.Request {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []micro.Request
	)
	svc, err := micro.AddService(nc, micro.Config{
		Name: "agents", Version: "1.0.0",
		Metadata: map[string]string{"agent": "stub", "owner": "t", "protocol_version": "0.3"},
	})
	if err != nil {
		t.Fatalf("micro.AddService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	err = svc.AddEndpoint("prompt", micro.HandlerFunc(func(req micro.Request) {
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		for _, c := range script(req) {
			_ = nc.Publish(req.Reply(), c)
		}
		_ = nc.Publish(req.Reply(), nil) // terminator
	}), micro.WithEndpointSubject(subject), micro.WithEndpointQueueGroup("agents"))
	if err != nil {
		t.Fatalf("AddEndpoint: %v", err)
	}
	return func() []micro.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]micro.Request(nil), reqs...)
	}
}

func chunk(typ string, data any) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	return b
}

// TestDispatch_SynadiaV03 proves a detected v0.3 agent: {prompt} request with
// turn headers, ack -> status -> response chunks -> terminator.
func TestDispatch_SynadiaV03(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.one"
	seen := v03Agent(t, nc, subject, func(micro.Request) [][]byte {
		return [][]byte{
			chunk("status", "ack"), chunk("status", "thinking"),
			chunk("response", "PO"), chunk("response", map[string]any{"text": "NG"}),
			chunk("future", "ignored"),
		}
	})
	c := dialShort(t, natsURL)

	sink := &recSink{}
	reply, err := c.Dispatch(domain.WithStatusSink(context.Background(), sink), historyTurn(subject))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply parts = %+v, want PONG", reply.Parts)
	}
	if got := sink.notes(); len(got) != 1 || got[0] != "thinking" {
		t.Errorf("notes = %v, want [thinking]", got)
	}
	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("agent saw %d requests, want 1", len(reqs))
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[0].Data(), &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if p, _ := body["prompt"].(string); !strings.Contains(p, "hello") || !strings.HasSuffix(p, "next") {
		t.Errorf("prompt = %q, want inline history then message", p)
	}
	h := reqs[0].Headers()
	if h.Get("Notify-Context-Id") != "conv-1" || h.Get("Notify-Message-Id") != "act-1" || h.Get("Notify-Subscriber-Id") != "sub-1" {
		t.Errorf("turn headers = %v", h)
	}
}

// TestDispatch_SynadiaV03HistoryNone sends only the new message.
func TestDispatch_SynadiaV03HistoryNone(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.two"
	seen := v03Agent(t, nc, subject, func(micro.Request) [][]byte {
		return [][]byte{chunk("response", "ok")}
	})
	c := dialShort(t, natsURL)
	tn := historyTurn(subject)
	tn.Agent.History = domain.HistoryNone

	if _, err := c.Dispatch(context.Background(), tn); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(seen()[0].Data(), &body)
	if body["prompt"] != "next" {
		t.Errorf("prompt = %v, want only the new message", body["prompt"])
	}
}

// TestDispatch_SynadiaV03QueryPermanent: a query chunk is unsupported.
func TestDispatch_SynadiaV03QueryPermanent(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.three"
	v03Agent(t, nc, subject, func(micro.Request) [][]byte {
		return [][]byte{chunk("status", "ack"), chunk("query", map[string]any{"id": "q1", "reply_subject": "_INBOX.x", "prompt": "ok?"})}
	})
	c := dialShort(t, natsURL)

	_, err := c.Dispatch(context.Background(), turn(subject, "hi"))
	if err == nil || !domain.IsPermanent(err) || !strings.Contains(err.Error(), "query") {
		t.Errorf("err = %v, want permanent error naming query", err)
	}
}

// TestDispatch_SynadiaV03ServiceError4xxPermanent: error headers surface as
// a permanent ServiceError.
func TestDispatch_SynadiaV03ServiceError4xxPermanent(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.four"
	v03Agent(t, nc, subject, func(req micro.Request) [][]byte {
		m := nats.NewMsg(req.Reply())
		m.Header.Set("Nats-Service-Error-Code", "400")
		m.Header.Set("Nats-Service-Error", "bad prompt")
		_ = nc.PublishMsg(m)
		return nil
	})
	c := dialShort(t, natsURL)

	_, err := c.Dispatch(context.Background(), turn(subject, "hi"))
	var se *mesh.ServiceError
	if !errors.As(err, &se) || se.Code != "400" || !domain.IsPermanent(err) {
		t.Errorf("err = %v, want permanent ServiceError 400", err)
	}
}

// scanCounter counts $SRV.INFO.agents requests seen on the hub.
func scanCounter(t *testing.T, nc *nats.Conn) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	sub, err := nc.Subscribe("$SRV.INFO.agents", func(*nats.Msg) { n.Add(1) })
	if err != nil {
		t.Fatalf("count subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
	return &n
}

func dialWith(t *testing.T, natsURL string, opts ...mesh.Option) *mesh.Client {
	t.Helper()
	c, err := mesh.Dial(natsURL, append([]mesh.Option{mesh.WithTimeout(5 * time.Second)}, opts...)...)
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestDispatch_SynadiaV03CachesDetection: after the first turn per subject,
// repeat turns never scan, and an a2a agent beside a v0.3 one still works.
func TestDispatch_SynadiaV03CachesDetection(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const v03Subject = "agents.prompt.stub.t.five"
	const a2aSubject = "agents.prompt.test.cache-a2a"
	v03Agent(t, nc, v03Subject, func(micro.Request) [][]byte {
		return [][]byte{chunk("response", "v03")}
	})
	stubAgent(t, nc, a2aSubject, pong)
	scans := scanCounter(t, nc)
	c := dialWith(t, natsURL, mesh.WithDiscoveryWindow(200*time.Millisecond))

	dispatch := func(i int) {
		t.Helper()
		r, err := c.Dispatch(context.Background(), turn(v03Subject, "hi"))
		if err != nil || r.Parts[0].Text != "v03" {
			t.Fatalf("v0.3 dispatch %d = %+v, %v", i, r, err)
		}
		r, err = c.Dispatch(context.Background(), turn(a2aSubject, "hi"))
		if err != nil || r.Parts[0].Text != "PONG" {
			t.Fatalf("a2a dispatch %d = %+v, %v", i, r, err)
		}
	}
	dispatch(0)
	_ = nc.Flush()
	before := scans.Load()
	for i := 1; i < 4; i++ {
		dispatch(i)
		// The a2a path has its own settle delay; time the v0.3 turn alone.
		start := time.Now()
		if _, err := c.Dispatch(context.Background(), turn(v03Subject, "hi")); err != nil {
			t.Fatalf("v0.3 dispatch %d: %v", i, err)
		}
		if d := time.Since(start); d > 150*time.Millisecond {
			t.Errorf("repeat turn %d took %v, want no scan wait", i, d)
		}
	}
	_ = nc.Flush()
	if n := scans.Load(); n != before {
		t.Errorf("repeat turns scanned: %d -> %d", before, n)
	}
}

// TestDispatch_SynadiaV03ExpiredEntryRefreshesInBackground: past the TTL a
// turn is answered from the stale entry at once and one scan refreshes it.
func TestDispatch_SynadiaV03ExpiredEntryRefreshesInBackground(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.expiry"
	v03Agent(t, nc, subject, func(micro.Request) [][]byte { return [][]byte{chunk("response", "v03")} })
	scans := scanCounter(t, nc)
	const window = time.Second
	c := dialWith(t, natsURL, mesh.WithDiscoveryWindow(window), mesh.WithDiscoveryTTL(100*time.Millisecond))
	if _, err := c.Dispatch(context.Background(), turn(subject, "hi")); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	time.Sleep(window + 200*time.Millisecond) // warm-up scan done, entry expired
	_ = nc.Flush()
	before := scans.Load()

	start := time.Now()
	r, err := c.Dispatch(context.Background(), turn(subject, "hi"))
	if elapsed := time.Since(start); err != nil || r.Parts[0].Text != "v03" || elapsed >= window {
		t.Fatalf("expired dispatch = %+v, %v after %v; want v03 well under %v", r, err, elapsed, window)
	}
	deadline := time.Now().Add(3 * time.Second)
	for scans.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := scans.Load(); n != before+1 {
		t.Errorf("refresh scans = %d, want 1", n-before)
	}
}

// TestDispatch_SynadiaV03ConcurrentColdMissesScanOnce: many turns for one
// unseen subject share a single scan.
func TestDispatch_SynadiaV03ConcurrentColdMissesScanOnce(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.test.cold"
	stubAgent(t, nc, subject, pong)
	scans := scanCounter(t, nc)
	const window = 200 * time.Millisecond
	c := dialWith(t, natsURL, mesh.WithDiscoveryWindow(window))
	time.Sleep(window + 150*time.Millisecond) // warm-up scan done
	_ = nc.Flush()
	before := scans.Load()

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Dispatch(context.Background(), turn(subject, "hi")); err != nil {
				t.Errorf("Dispatch: %v", err)
			}
		}()
	}
	wg.Wait()
	_ = nc.Flush()
	if got := scans.Load() - before; got != 1 {
		t.Errorf("scans for %d concurrent cold misses = %d, want 1", n, got)
	}
}

// TestDispatch_SynadiaV03WarmUp: an agent already serving at Dial is detected
// by the warm-up scan, so the first turn does not wait out the window.
func TestDispatch_SynadiaV03WarmUp(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const subject = "agents.prompt.stub.t.warm"
	seen := v03Agent(t, nc, subject, func(micro.Request) [][]byte { return [][]byte{chunk("response", "v03")} })
	const window = 2 * time.Second
	c := dialWith(t, natsURL, mesh.WithDiscoveryWindow(window))

	start := time.Now()
	r, err := c.Dispatch(context.Background(), turn(subject, "hi"))
	if elapsed := time.Since(start); err != nil || r.Parts[0].Text != "v03" || elapsed >= window/2 {
		t.Fatalf("first dispatch = %+v, %v after %v; want v03 without the window", r, err, elapsed)
	}
	if len(seen()) != 1 {
		t.Errorf("agent saw %d requests, want 1", len(seen()))
	}
}
