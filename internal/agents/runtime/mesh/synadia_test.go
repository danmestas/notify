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

	reply, err := c.Dispatch(context.Background(), historyTurn(subject))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) != 1 || reply.Parts[0].Text != "PONG" {
		t.Errorf("reply parts = %+v, want PONG", reply.Parts)
	}
	if len(reply.Status) != 1 || reply.Status[0] != "thinking" {
		t.Errorf("reply status = %v, want [thinking]", reply.Status)
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

// TestDispatch_SynadiaV03CachesDetection: detection scans $SRV.INFO once per
// subject per TTL, not once per turn, and an a2a agent beside a v0.3 one still works.
func TestDispatch_SynadiaV03CachesDetection(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	nc := connect(t, natsURL)
	const v03Subject = "agents.prompt.stub.t.five"
	const a2aSubject = "agents.prompt.test.cache-a2a"
	v03Agent(t, nc, v03Subject, func(micro.Request) [][]byte {
		return [][]byte{chunk("response", "v03")}
	})
	stubAgent(t, nc, a2aSubject, pong)
	var scans atomic.Int32
	sub, err := nc.Subscribe("$SRV.INFO.agents", func(*nats.Msg) { scans.Add(1) })
	if err != nil {
		t.Fatalf("count subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	c, err := mesh.Dial(natsURL, mesh.WithTimeout(5*time.Second), mesh.WithDiscoveryWindow(200*time.Millisecond))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	defer c.Close()

	for i := 0; i < 3; i++ {
		r, err := c.Dispatch(context.Background(), turn(v03Subject, "hi"))
		if err != nil || r.Parts[0].Text != "v03" {
			t.Fatalf("v0.3 dispatch %d = %+v, %v", i, r, err)
		}
		r, err = c.Dispatch(context.Background(), turn(a2aSubject, "hi"))
		if err != nil || r.Parts[0].Text != "PONG" {
			t.Fatalf("a2a dispatch %d = %+v, %v", i, r, err)
		}
	}
	// One scan per distinct subject (the a2a one is absent from the first
	// scan's results, and a new agent must be found), none on repeat turns.
	if n := scans.Load(); n != 2 {
		t.Errorf("$SRV.INFO.agents scans = %d, want 2", n)
	}
}
