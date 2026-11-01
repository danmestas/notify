package httpapi_test

// Slice A6: end-to-end + adversarial multi-tenant proof for the stream
// channel. Exercises the full shipped stack — store + eventbus.Bus + engine
// + trigger + authoring + identity + httpapi + feedapi + webhooksout —
// against a real dagnats sidecar, mirroring internal/httpapi/email_parity_e2e_test.go's
// harness shape. Tests only: no production code lives in this file.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/feedapi"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/webhooksout"
)

// wireStreamHandler registers send.stream on eng using the exact .With*
// chain shape notify/wiring.go's wireCommon expands to for send.stream:
//
//	wireCommon(delivery.NewStream(st), d).WithFeed(bus).HandleStream
//	  == delivery.NewStream(st).WithPreferences(d.prefs).WithEventBus(d.bus).
//	     WithThrottle(d.bus).WithDigester(d.bus).WithFeed(bus).HandleStream
//
// This harness carries no preferences.Service, so WithPreferences receives
// nil — delivery's documented default (a nil PreferenceChecker always sends).
func wireStreamHandler(eng *engine.Engine, st *store.Store, bus *eventbus.Bus) {
	eng.Handle("notify.send.stream", delivery.NewStream(st).
		WithPreferences(nil).
		WithEventBus(bus).
		WithThrottle(bus).
		WithDigester(bus).
		WithFeed(bus).
		HandleStream)
}

// sseFrame is one parsed "id: <n>\ndata: <payload>\n\n" SSE frame. Mirrors
// internal/feedapi's own (unexported) test helper of the same shape; kept
// separate here because this file drives the endpoint over a real HTTP
// client with an Authorization header instead of a scope-injecting
// middleware stub.
type sseFrame struct {
	id   string
	data string
}

// sseStream wraps an open SSE connection with a single scanner goroutine
// feeding a channel, so tests can read frames incrementally without racing
// two scanners over the same response body.
type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
}

// read blocks for n frames or fails the test after timeout.
func (s *sseStream) read(t *testing.T, n int, timeout time.Duration) []sseFrame {
	t.Helper()
	var got []sseFrame
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatalf("stream closed after %d/%d frames: %v", len(got), n, got)
			}
			got = append(got, f)
		case <-deadline:
			t.Fatalf("timed out waiting for %d SSE frames, got %d: %v", n, len(got), got)
		}
	}
	return got
}

// expectNoMore asserts no further frame arrives within d.
func (s *sseStream) expectNoMore(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			t.Fatalf("received unexpected extra frame: %+v", f)
		}
	case <-time.After(d):
	}
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// openFeedStream opens a real authenticated GET /v1/feed/stream connection
// against baseURL (an httptest server wrapping httpapi.New with a real
// identity.Middleware + feedapi.Gateway) and returns an sseStream reading
// parsed frames off it. It fatals the test if the response is not 200 —
// callers expecting a non-200 (bad input, no auth) should use doGet instead,
// which does not block on a streaming body.
func openFeedStream(t *testing.T, baseURL, authHeader, query string) *sseStream {
	t.Helper()
	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/v1/feed/stream?"+query, nil)
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by sseStream.close()
	if err != nil {
		cancel()
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}

	frames := make(chan sseFrame, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if cur.data != "" {
					frames <- cur
					cur = sseFrame{}
				}
			}
		}
	}()

	return &sseStream{resp: resp, frames: frames, cancel: cancel}
}

// decodeMessage unmarshals an SSE frame's data payload as the persisted
// domain.Message it carries.
func decodeMessage(t *testing.T, data string) domain.Message {
	t.Helper()
	var m domain.Message
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("decode frame as domain.Message: %v (data=%s)", err, data)
	}
	return m
}

// TestStream_TriggerToSSE_EndToEnd is the Slice A6 headline proof: the full
// shipped stack drives a triggered stream send from HTTP trigger through to
// an SSE frame, a persisted domain.Message, a core-NATS SubscribeDelivery
// subscriber (live-tail/analytics parity), and webhooksout's durable
// consumer (outbound-webhook parity) — all against a real dagnats sidecar.
func TestStream_TriggerToSSE_EndToEnd(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	// Register handlers before StartWorkers, mirroring cmd/notify/main.go's
	// ordering requirement.
	wireStreamHandler(eng, st, bus)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	feed := feedapi.New(bus)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	authHeader := "ApiKey " + key

	controls := json.RawMessage(`{"topic":"orders.{{payload.region}}","body":"#{{payload.n}}"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Order Stream",
		TriggerIdentifier: "order-stream",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "stream",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "order-stream"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	// Second durable consumer: webhooksout's real Consumer against an
	// httptest endpoint, reusing the B2 harness pattern
	// (internal/webhooksout/consumer_test.go) to prove outbound-webhook
	// parity.
	whReceived := make(chan []byte, 4)
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		whReceived <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(whSrv.Close)
	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: whSrv.URL, Active: true}); err != nil {
		t.Fatalf("WebhookEndpoints().Create: %v", err)
	}
	whConsumer := webhooksout.NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second})
	whStop, err := whConsumer.Start(ctx)
	if err != nil {
		t.Fatalf("webhooksout Start: %v", err)
	}
	t.Cleanup(func() { _ = whStop() })

	// Core-NATS SubscribeDelivery subscriber: live-tail/analytics parity
	// (B1's regression guard, exercised here end-to-end).
	deliveryCh := make(chan []byte, 4)
	unsub, err := bus.SubscribeDelivery(scope.EnvID, func(data []byte) { deliveryCh <- data })
	if err != nil {
		t.Fatalf("SubscribeDelivery: %v", err)
	}
	t.Cleanup(func() { _ = unsub() })

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Feed: feed}))
	t.Cleanup(srv.Close)

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"user_1"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"order-stream","to":{"subscriberId":"user_1"},"payload":{"region":"eu","n":1},"transactionId":"stx-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}
	var trigResp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &trigResp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	if !trigResp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}

	// SSE oracle: connect after the trigger and replay from the start of the
	// feed (from=0) so backlog delivery — the durable feed's core promise —
	// is exercised rather than racing a live-only connection against the
	// trigger.
	stream := openFeedStream(t, srv.URL, authHeader, "topic=orders.eu&from=0")
	t.Cleanup(stream.close)
	frames := stream.read(t, 1, 15*time.Second)
	msg := decodeMessage(t, frames[0].data)
	if msg.Channel != "stream" {
		t.Errorf("Channel = %q, want %q", msg.Channel, "stream")
	}
	if msg.Subject != "orders.eu" {
		t.Errorf("Subject = %q, want %q", msg.Subject, "orders.eu")
	}
	if msg.Content != "#1" {
		t.Errorf("Content = %q, want %q", msg.Content, "#1")
	}
	if msg.Status != "sent" {
		t.Errorf("Status = %q, want %q", msg.Status, "sent")
	}

	// Store oracle: the message row is actually persisted.
	stored, err := st.Messages().List(ctx, scope, "user_1", "stream")
	if err != nil {
		t.Fatalf("Messages().List: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored stream messages = %d, want 1 (%+v)", len(stored), stored)
	}
	if stored[0].ID != msg.ID {
		t.Errorf("stored message ID = %q, SSE frame message ID = %q, want match", stored[0].ID, msg.ID)
	}

	// Delivery-status oracle.
	select {
	case data := <-deliveryCh:
		var dm domain.Message
		if err := json.Unmarshal(data, &dm); err != nil {
			t.Fatalf("decode delivery event: %v", err)
		}
		if dm.Channel != "stream" || dm.ID != msg.ID {
			t.Errorf("delivery event = %+v, want the same stream message", dm)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery-status event reached the SubscribeDelivery subscriber")
	}

	// Webhooksout oracle.
	select {
	case body := <-whReceived:
		var wm domain.Message
		if err := json.Unmarshal(body, &wm); err != nil {
			t.Fatalf("decode webhook body: %v", err)
		}
		if wm.Channel != "stream" || wm.ID != msg.ID {
			t.Errorf("webhook body = %+v, want the same stream message", wm)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("webhooksout durable consumer never POSTed the stream delivery event")
	}
}

// TestStream_ReplayAfterFullRestart proves the "replayable durable feed"
// claim across a process-level reconnect: it triggers two stream sends,
// tears down BOTH the SSE client and the eventbus.Bus, reconnects a fresh
// Bus to the same sidecar, and asserts a brand-new SSE connection (from=0)
// replays both messages. A merely in-memory fan-out (core NATS) could not
// survive this — the durable NOTIFY_FEED stream is what does.
func TestStream_ReplayAfterFullRestart(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}

	wireStreamHandler(eng, st, bus)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	feed := feedapi.New(bus)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	authHeader := "ApiKey " + key

	controls := json.RawMessage(`{"topic":"events","body":"#{{payload.n}}"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Event Stream",
		TriggerIdentifier: "event-stream",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "stream",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "event-stream"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Feed: feed}))

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"user_2"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	for i, tx := range []string{"rtx-1", "rtx-2"} {
		body := fmt.Sprintf(`{"name":"event-stream","to":{"subscriberId":"user_2"},"payload":{"n":%d},"transactionId":%q}`, i+1, tx)
		if status, respBody := doPost(t, srv.URL+"/v1/events/trigger", authHeader, body); status != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger(%d): status=%d body=%s", i, status, respBody)
		}
	}

	// Prime the durable feed BEFORE tearing anything down: open an SSE
	// connection against the ORIGINAL server (from=0) and read both frames.
	// HandleStream only calls PublishFeed after the message is durably
	// persisted (internal/delivery/stream.go), and this read replays
	// straight off the durable NOTIFY_FEED stream — the same path the
	// post-restart assertion below exercises — so seeing both frames here
	// proves both sends are durably committed, not a race against send
	// latency, before the bus/process-level restart happens.
	priming := openFeedStream(t, srv.URL, authHeader, "topic=events&from=0")
	priming.read(t, 2, 15*time.Second)
	priming.close()

	// Tear down the SSE client's server AND the Bus — simulating a full
	// notify-process restart, not just a dropped HTTP connection.
	srv.Close()
	if err := bus.Close(); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}

	// Reconnect a FRESH Bus to the SAME sidecar.
	bus2, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect (restart): %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close() })
	feed2 := feedapi.New(bus2)

	srv2 := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Feed: feed2}))
	t.Cleanup(srv2.Close)

	stream := openFeedStream(t, srv2.URL, authHeader, "topic=events&from=0")
	t.Cleanup(stream.close)
	frames := stream.read(t, 2, 15*time.Second)

	// The two triggers are dispatched to independent engine worker
	// goroutines (the engine runs tasks of one type concurrently), so
	// their PublishFeed calls — and therefore the durable stream's sequence
	// order — can land in either order, regardless of HTTP trigger order.
	// Assert on the SET of replayed contents (both sends survived the
	// restart), not which one arrives first.
	gotContents := map[string]bool{}
	for _, f := range frames {
		gotContents[decodeMessage(t, f.data).Content] = true
	}
	if !gotContents["#1"] || !gotContents["#2"] {
		t.Fatalf("replayed contents = %v, want both #1 and #2", gotContents)
	}
	if frames[0].id == "" || frames[1].id == "" || frames[0].id == frames[1].id {
		t.Fatalf("replayed frame ids = %q, %q, want distinct non-empty stream seqs", frames[0].id, frames[1].id)
	}
}

// TestStream_CrossTenantCannotRead is the adversarial multi-tenant proof:
// two tenants share one deployment (store + bus + engine); tenant A
// triggers a stream send, and tenant B's API key — reading the exact same
// topic string, a topic crafted to literally spell tenant A's subject
// prefix, and outright wildcard topics — never sees any of A's data.
func TestStream_CrossTenantCannotRead(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	wireStreamHandler(eng, st, bus)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	feed := feedapi.New(bus)

	keyA, scopeA, err := id.Bootstrap(ctx, "TenantA", "dev")
	if err != nil {
		t.Fatalf("Bootstrap A: %v", err)
	}
	keyB, _, err := id.Bootstrap(ctx, "TenantB", "dev")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authHeaderA := "ApiKey " + keyA
	authHeaderB := "ApiKey " + keyB

	controls := json.RawMessage(`{"topic":"orders.{{payload.region}}","body":"leaked?"}`)
	if _, err := auth.Create(ctx, scopeA, authoring.WorkflowInput{
		Name:              "Order Stream",
		TriggerIdentifier: "order-stream",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "stream",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scopeA, "order-stream"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Feed: feed}))
	t.Cleanup(srv.Close)

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeaderA, `{"subscriberId":"user_a"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers (A): status=%d body=%s", status, body)
	}

	trigBody := `{"name":"order-stream","to":{"subscriberId":"user_a"},"payload":{"region":"eu"},"transactionId":"ctx-1"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeaderA, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	// 1) B connects to the exact same topic string A published to
	// ("orders.eu"). The env comes only from B's authenticated scope, so B
	// gets its OWN (empty) feed, never A's.
	streamSame := openFeedStream(t, srv.URL, authHeaderB, "topic=orders.eu&from=0")
	streamSame.expectNoMore(t, 3*time.Second)
	streamSame.close()

	// 2) B crafts a topic that literally spells A's subject prefix. Dots are
	// legal topic characters, so this passes edge validation as an ordinary
	// (if odd-looking) topic string — but ConsumeFeed filters on
	// notify.feed.<B's envID>.<topic>, which becomes
	// notify.feed.<envB>.notify.feed.<envA>.orders: a subject nothing A ever
	// published to.
	crafted := "notify.feed." + scopeA.EnvID + ".orders"
	streamCrafted := openFeedStream(t, srv.URL, authHeaderB, "topic="+url.QueryEscape(crafted)+"&from=0")
	streamCrafted.expectNoMore(t, 3*time.Second)
	streamCrafted.close()

	// 3) Outright wildcards are rejected at the edge, not silently scoped.
	for _, topic := range []string{"*", ">"} {
		status, body := doGet(t, srv.URL+"/v1/feed/stream?topic="+url.QueryEscape(topic), authHeaderB)
		if status != http.StatusBadRequest {
			t.Errorf("topic=%q: status = %d, want 400 (body=%s)", topic, status, body)
		}
	}
}

// TestStream_TopicInjectionViaPayload documents the rendered-topic hard gate
// (A4's domain.ValidateFeedTopic, applied before any publish): a payload
// token that renders a wildcard character into the topic template fails
// closed into a persisted "error" message and never reaches PublishFeed,
// while a payload token that merely ADDS topic segments (multi-token,
// wildcard-free) is legal and publishes normally.
func TestStream_TopicInjectionViaPayload(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	wireStreamHandler(eng, st, bus)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	feed := feedapi.New(bus)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	authHeader := "ApiKey " + key

	controls := json.RawMessage(`{"topic":"orders.{{payload.region}}","body":"body"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Order Stream",
		TriggerIdentifier: "order-stream",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "stream",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "order-stream"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Feed: feed}))
	t.Cleanup(srv.Close)

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"user_4"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	fire := func(region, tx string) {
		body := fmt.Sprintf(`{"name":"order-stream","to":{"subscriberId":"user_4"},"payload":{"region":%q},"transactionId":%q}`, region, tx)
		if status, respBody := doPost(t, srv.URL+"/v1/events/trigger", authHeader, body); status != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger(region=%s): status=%d body=%s", region, status, respBody)
		}
	}

	// region="eu.*" and region=">" each render a wildcard token into the
	// topic; both must fail closed.
	fire("eu.*", "inj-1")
	fire(">", "inj-2")
	// region="x.y" merely ADDS a topic segment (multi-token render is
	// allowed) — legal, and must publish normally.
	fire("x.y", "inj-3")

	var stored []domain.Message
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.Messages().List(ctx, scope, "user_4", "stream")
		if err != nil {
			t.Fatalf("Messages().List: %v", err)
		}
		if len(got) >= 3 {
			stored = got
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(stored) < 3 {
		t.Fatalf("stored stream messages = %d, want 3", len(stored))
	}

	byTx := map[string]domain.Message{}
	for _, m := range stored {
		byTx[m.TransactionID] = m
	}

	for _, tx := range []string{"inj-1", "inj-2"} {
		m, ok := byTx[tx]
		if !ok {
			t.Fatalf("no stored message for transactionId %q", tx)
		}
		if m.Status != "error" {
			t.Errorf("%s: Status = %q, want %q", tx, m.Status, "error")
		}
		if !strings.Contains(m.ErrorText, "topic") {
			t.Errorf("%s: ErrorText = %q, want it to mention the topic", tx, m.ErrorText)
		}
	}

	legal, ok := byTx["inj-3"]
	if !ok {
		t.Fatalf("no stored message for transactionId %q", "inj-3")
	}
	if legal.Status != "sent" {
		t.Fatalf("inj-3: Status = %q, want %q", legal.Status, "sent")
	}
	if legal.Subject != "orders.x.y" {
		t.Fatalf("inj-3: Subject = %q, want %q", legal.Subject, "orders.x.y")
	}

	// The feed side: only the legal render published. Connecting to its
	// concrete topic (orders.x.y) from the start of the feed returns
	// exactly that one message; the HTTP surface offers no way to ask "did
	// anything publish to orders.eu.* or orders.>" — wildcard topics are
	// rejected at the edge by construction, which is itself part of the
	// isolation guarantee this test documents.
	stream := openFeedStream(t, srv.URL, authHeader, "topic=orders.x.y&from=0")
	t.Cleanup(stream.close)
	frames := stream.read(t, 1, 10*time.Second)
	msg := decodeMessage(t, frames[0].data)
	if msg.TransactionID != "inj-3" {
		t.Fatalf("feed frame TransactionID = %q, want %q", msg.TransactionID, "inj-3")
	}
	stream.expectNoMore(t, 2*time.Second)
}
