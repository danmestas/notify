package feedingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/feedapi"
	"github.com/danmestas/notify/internal/feedingest"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/streamsource"
)

// env is a test-local decode of the §0.4 ingest envelope.
type env struct {
	V        int             `json:"v"`
	SourceID string          `json:"source_id"`
	EventID  string          `json:"event_id"`
	TS       string          `json:"ts"`
	Data     json.RawMessage `json:"data"`
}

// harness wires a real sidecar-backed bus, a streamsource service over a
// sqlite store, and an httptest server mounting only the ingest gateway.
type harness struct {
	bus *eventbus.Bus
	svc *streamsource.Service
	url string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	natsURL := dagtest.StartSidecar(t)
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

	key := bytes.Repeat([]byte{0x2a}, 32)
	svc := streamsource.New(st, key)
	gw := feedingest.New(svc, bus)

	r := chi.NewRouter()
	gw.Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &harness{bus: bus, svc: svc, url: srv.URL}
}

// ingResp is a fully-read, already-closed response — no *http.Response escapes
// the helper, so bodyclose has nothing to (falsely) flag at the call sites.
type ingResp struct {
	Status int
	Header http.Header
	Body   string
}

func (h *harness) post(t *testing.T, sourceID, secret, body string) ingResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+"/v1/sources/"+sourceID+"/events", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "SourceKey "+secret)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return ingResp{Status: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

// collectAll consumes the (envID, topic) feed from seq 1 for dur and returns
// every envelope observed, in stream order.
func collectAll(t *testing.T, bus *eventbus.Bus, envID, topic string, dur time.Duration) []env {
	t.Helper()
	ch := make(chan []byte, 256)
	stop, err := bus.ConsumeFeed(context.Background(), envID, topic, 1, time.Time{}, func(m eventbus.FeedMsg) {
		cp := make([]byte, len(m.Data))
		copy(cp, m.Data)
		ch <- cp
	})
	if err != nil {
		t.Fatalf("ConsumeFeed(%s,%s): %v", envID, topic, err)
	}
	defer func() { _ = stop() }()

	var out []env
	deadline := time.After(dur)
	for {
		select {
		case d := <-ch:
			var e env
			if err := json.Unmarshal(d, &e); err != nil {
				t.Fatalf("unmarshal envelope: %v (%s)", err, string(d))
			}
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
}

func mustCreate(t *testing.T, svc *streamsource.Service, scope domain.Scope, name, topic string) (domain.StreamSource, string) {
	t.Helper()
	src, raw, err := svc.Create(context.Background(), scope, name, topic)
	if err != nil {
		t.Fatalf("Create(%s,%s): %v", name, topic, err)
	}
	return src, raw
}

// dataN extracts the numeric "n" field from an envelope's data payload.
func dataN(t *testing.T, e env) int {
	t.Helper()
	var m struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return m.N
}

// TestIngest_HappyPath_BatchToFeed — a 3-event batch lands as 3 §0.4 envelopes
// in order on the source's feed topic.
func TestIngest_HappyPath_BatchToFeed(t *testing.T) {
	h := newHarness(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "agent.tokens")

	body := `{"events":[{"data":{"n":1}},{"data":{"n":2}},{"data":{"n":3}}]}`
	res := h.post(t, src.ID, secret, body)
	if res.Status != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", res.Status, res.Body)
	}
	var ar struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal([]byte(res.Body), &ar); err != nil {
		t.Fatalf("decode response: %v (%s)", err, res.Body)
	}
	if ar.Accepted != 3 {
		t.Fatalf("accepted = %d, want 3", ar.Accepted)
	}

	got := collectAll(t, h.bus, src.EnvID, "agent.tokens", 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("consumed %d envelopes, want 3", len(got))
	}
	for i, e := range got {
		if e.V != 1 {
			t.Fatalf("envelope[%d].v = %d, want 1", i, e.V)
		}
		if e.SourceID != src.ID {
			t.Fatalf("envelope[%d].source_id = %q, want %q", i, e.SourceID, src.ID)
		}
		if e.EventID == "" || e.TS == "" {
			t.Fatalf("envelope[%d] missing event_id/ts: %+v", i, e)
		}
		if n := dataN(t, e); n != i+1 {
			t.Fatalf("envelope[%d] data.n = %d, want %d (order not preserved)", i, n, i+1)
		}
	}
}

// TestIngest_AuthMatrix — the SourceKey scheme is required and distinct; a
// valid secret must not confirm another source's id.
func TestIngest_AuthMatrix(t *testing.T) {
	h := newHarness(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "orders")
	body := `{"events":[{"data":{"n":1}}]}`

	// No Authorization header → 401.
	if res := h.post(t, src.ID, "", body); res.Status != http.StatusUnauthorized {
		t.Fatalf("no header: status = %d (%s), want 401", res.Status, res.Body)
	}
	// Console ApiKey scheme on the ingest endpoint → 401 (scheme mismatch).
	req, _ := http.NewRequest(http.MethodPost, h.url+"/v1/sources/"+src.ID+"/events", strings.NewReader(body))
	req.Header.Set("Authorization", "ApiKey "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ApiKey scheme: status = %d, want 401", resp.StatusCode)
	}
	// Wrong (valid-hex, unknown) secret → 401.
	if res := h.post(t, src.ID, "deadbeefdeadbeef", body); res.Status != http.StatusUnauthorized {
		t.Fatalf("wrong secret: status = %d, want 401", res.Status)
	}
	// Valid secret, wrong path {sourceID} → 404.
	if res := h.post(t, "01SOMEOTHERSOURCEID000000", secret, body); res.Status != http.StatusNotFound {
		t.Fatalf("path mismatch: status = %d, want 404", res.Status)
	}
	// Inactive source → 403.
	if err := h.svc.SetActive(context.Background(), scope, src.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if res := h.post(t, src.ID, secret, body); res.Status != http.StatusForbidden {
		t.Fatalf("inactive: status = %d, want 403", res.Status)
	}
	if err := h.svc.SetActive(context.Background(), scope, src.ID, true); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	// Rotated-away secret → 401 (old secret no longer resolves).
	if _, err := h.svc.RotateSecret(context.Background(), scope, src.ID); err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if res := h.post(t, src.ID, secret, body); res.Status != http.StatusUnauthorized {
		t.Fatalf("rotated-away secret: status = %d, want 401", res.Status)
	}
}

// TestIngest_TopicScoping — a request topic must equal the source topic or
// extend it as a dot-prefix; every other shape is a 400 that publishes nothing.
func TestIngest_TopicScoping(t *testing.T) {
	h := newHarness(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "orders")

	// Exact topic → ok.
	if res := h.post(t, src.ID, secret, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("exact topic: status = %d (%s), want 202", res.Status, res.Body)
	}
	// Dot-prefix extension → ok, lands on the extended subject.
	if res := h.post(t, src.ID, secret, `{"topic":"orders.tokens","events":[{"data":{"n":2}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("extended topic: status = %d (%s), want 202", res.Status, res.Body)
	}
	if got := collectAll(t, h.bus, src.EnvID, "orders.tokens", 2*time.Second); len(got) != 1 {
		t.Fatalf("orders.tokens received %d envelopes, want 1", len(got))
	}

	// Rejected topics: sibling, parent, prefix-trick, wildcard, >, system.
	for _, bad := range []string{"other", "order", "ordersx", "orders.*", ">", "$JS.api"} {
		body := fmt.Sprintf(`{"topic":%q,"events":[{"data":{"n":9}}]}`, bad)
		if res := h.post(t, src.ID, secret, body); res.Status != http.StatusBadRequest {
			t.Fatalf("topic %q: status = %d (%s), want 400", bad, res.Status, res.Body)
		}
		// The bad topic (when ValidateFeedTopic-legal, e.g. "other"/"ordersx")
		// must have published nothing to its own subject.
		if err := domain.ValidateFeedTopic(bad); err == nil {
			if got := collectAll(t, h.bus, src.EnvID, bad, time.Second); len(got) != 0 {
				t.Fatalf("rejected topic %q published %d events, want 0", bad, len(got))
			}
		}
	}
}

// TestIngest_CrossSourceIsolation — two sources in two envs sharing a topic
// string are subject-disjoint: A's publish never reaches env B.
func TestIngest_CrossSourceIsolation(t *testing.T) {
	h := newHarness(t)
	scopeA := domain.Scope{OrgID: "o1", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "o2", EnvID: "envB"}
	srcA, secretA := mustCreate(t, h.svc, scopeA, "relay", "shared")
	_, _ = mustCreate(t, h.svc, scopeB, "relay", "shared")

	if res := h.post(t, srcA.ID, secretA, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("A publish: status = %d (%s), want 202", res.Status, res.Body)
	}
	if got := collectAll(t, h.bus, "envA", "shared", 2*time.Second); len(got) != 1 {
		t.Fatalf("env A received %d, want 1", len(got))
	}
	if got := collectAll(t, h.bus, "envB", "shared", time.Second); len(got) != 0 {
		t.Fatalf("env B received %d of A's events, want 0 (isolation breach)", len(got))
	}
}

// TestIngest_ShapeCaps — batch/event/body caps and malformed bodies are
// rejected before any publish.
func TestIngest_ShapeCaps(t *testing.T) {
	h := newHarness(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "orders")
	// Tighten the per-event cap so a modest payload trips it.
	if err := h.svc.UpdateLimits(context.Background(), scope, src.ID, 600, 16, 100, 1<<20); err != nil {
		t.Fatalf("UpdateLimits: %v", err)
	}

	// Batch of MaxBatchEvents+1 → 413.
	var b strings.Builder
	b.WriteString(`{"events":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"data":{"n":1}}`)
	}
	b.WriteString(`]}`)
	if res := h.post(t, src.ID, secret, b.String()); res.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch: status = %d, want 413", res.Status)
	}

	// Single event data over MaxEventBytes (16) → 413.
	big := `{"events":[{"data":{"v":"` + strings.Repeat("x", 40) + `"}}]}`
	if res := h.post(t, src.ID, secret, big); res.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized event: status = %d, want 413", res.Status)
	}

	// Request body over 1 MiB → 413.
	huge := `{"events":[{"data":{"v":"` + strings.Repeat("y", (1<<20)+16) + `"}}]}`
	if res := h.post(t, src.ID, secret, huge); res.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d, want 413", res.Status)
	}

	// Invalid JSON → 400.
	if res := h.post(t, src.ID, secret, `{"events":[`); res.Status != http.StatusBadRequest {
		t.Fatalf("invalid JSON: status = %d, want 400", res.Status)
	}
	// Empty events → 400.
	if res := h.post(t, src.ID, secret, `{"events":[]}`); res.Status != http.StatusBadRequest {
		t.Fatalf("empty events: status = %d, want 400", res.Status)
	}
	// Bad ts → 400.
	if res := h.post(t, src.ID, secret, `{"events":[{"ts":"not-a-time","data":{"n":1}}]}`); res.Status != http.StatusBadRequest {
		t.Fatalf("bad ts: status = %d, want 400", res.Status)
	}
	// 200-char id → 400.
	longID := strings.Repeat("a", 200)
	if res := h.post(t, src.ID, secret, `{"events":[{"id":"`+longID+`","data":{"n":1}}]}`); res.Status != http.StatusBadRequest {
		t.Fatalf("overlong id: status = %d, want 400", res.Status)
	}

	// Nothing should have been published across all the rejected requests.
	if got := collectAll(t, h.bus, src.EnvID, "orders", time.Second); len(got) != 0 {
		t.Fatalf("rejected requests published %d events, want 0", len(got))
	}
}

// TestIngest_BadTS_RejectsBeforeBudgetSpend — a malformed ts must be rejected
// in the shape-caps stage, BEFORE the rate-limit/byte-budget BudgetAllow gates
// record a spend. Proven by tightening the rate limit to exactly one event per
// minute, sending a one-event batch with a bad ts (400), then immediately
// sending a one-event batch with a valid ts: if the bad-ts request had already
// consumed the single-event budget, this second request would 429.
func TestIngest_BadTS_RejectsBeforeBudgetSpend(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "orders")
	if err := h.svc.UpdateLimits(ctx, scope, src.ID, 1, 32768, 100, 1<<20); err != nil {
		t.Fatalf("UpdateLimits: %v", err)
	}

	// Bad ts → 400, before any BudgetAllow spend.
	bad := `{"events":[{"ts":"not-a-time","data":{"n":1}}]}`
	if res := h.post(t, src.ID, secret, bad); res.Status != http.StatusBadRequest {
		t.Fatalf("bad ts: status = %d (%s), want 400", res.Status, res.Body)
	}

	// A valid one-event batch right after must still be within the 1/min
	// budget — proof the bad-ts request did not spend it.
	good := `{"events":[{"data":{"n":1}}]}`
	if res := h.post(t, src.ID, secret, good); res.Status != http.StatusAccepted {
		t.Fatalf("valid batch after bad ts: status = %d (%s), want 202 (budget was spent by the rejected bad-ts request)", res.Status, res.Body)
	}

	got := collectAll(t, h.bus, src.EnvID, "orders", time.Second)
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1 (only the valid batch)", len(got))
	}
}

// TestIngest_RateAndBudget429 — event-rate and byte-budget exhaustion return
// 429 with Retry-After and publish nothing.
func TestIngest_RateAndBudget429(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Rate limit: 5 events/min. Six single-event requests → the 6th is 429.
	scopeR := domain.Scope{OrgID: "o1", EnvID: "envR"}
	srcR, secretR := mustCreate(t, h.svc, scopeR, "relay", "rate")
	if err := h.svc.UpdateLimits(ctx, scopeR, srcR.ID, 5, 32768, 100, 1<<20); err != nil {
		t.Fatalf("UpdateLimits rate: %v", err)
	}
	for i := 0; i < 5; i++ {
		if res := h.post(t, srcR.ID, secretR, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusAccepted {
			t.Fatalf("rate req %d: status = %d (%s), want 202", i+1, res.Status, res.Body)
		}
	}
	res := h.post(t, srcR.ID, secretR, `{"events":[{"data":{"n":1}}]}`)
	if res.Status != http.StatusTooManyRequests {
		t.Fatalf("6th rate req: status = %d, want 429", res.Status)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatalf("429 missing Retry-After header")
	}

	// Byte budget: 100 bytes/hour. One event whose data exceeds it → 429.
	scopeB := domain.Scope{OrgID: "o2", EnvID: "envB"}
	srcB, secretB := mustCreate(t, h.svc, scopeB, "relay", "bytes")
	if err := h.svc.UpdateLimits(ctx, scopeB, srcB.ID, 600, 32768, 100, 100); err != nil {
		t.Fatalf("UpdateLimits budget: %v", err)
	}
	over := `{"events":[{"data":{"v":"` + strings.Repeat("z", 200) + `"}}]}`
	resB := h.post(t, srcB.ID, secretB, over)
	if resB.Status != http.StatusTooManyRequests {
		t.Fatalf("byte budget: status = %d, want 429", resB.Status)
	}
	if resB.Header.Get("Retry-After") == "" {
		t.Fatalf("byte-budget 429 missing Retry-After header")
	}

	// Neither denied request published anything.
	if got := collectAll(t, h.bus, "envR", "rate", time.Second); len(got) != 5 {
		t.Fatalf("rate env published %d events, want exactly the 5 allowed", len(got))
	}
	if got := collectAll(t, h.bus, "envB", "bytes", time.Second); len(got) != 0 {
		t.Fatalf("byte-budget env published %d events, want 0", len(got))
	}
}

// TestIngest_DedupOnRetry — re-POSTing a batch that carries producer ids is
// absorbed by the JetStream dedup window: each event lands exactly once.
func TestIngest_DedupOnRetry(t *testing.T) {
	h := newHarness(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "orders")

	body := `{"events":[{"id":"e1","data":{"n":1}},{"id":"e2","data":{"n":2}}]}`
	if res := h.post(t, src.ID, secret, body); res.Status != http.StatusAccepted {
		t.Fatalf("first POST: status = %d (%s), want 202", res.Status, res.Body)
	}
	// Retry the identical batch — the producer's whole-batch retry.
	if res := h.post(t, src.ID, secret, body); res.Status != http.StatusAccepted {
		t.Fatalf("retry POST: status = %d (%s), want 202", res.Status, res.Body)
	}

	got := collectAll(t, h.bus, src.EnvID, "orders", 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("consumed %d envelopes after retry, want 2 (dedup)", len(got))
	}
}

// TestIngest_TouchActivity — LastEventAt is set after a success; LastError is
// set after a rejected request.
func TestIngest_TouchActivity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "envA"}
	src, secret := mustCreate(t, h.svc, scope, "relay", "orders")

	if res := h.post(t, src.ID, secret, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("success POST: status = %d (%s), want 202", res.Status, res.Body)
	}
	got, err := h.svc.Get(ctx, scope, src.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastEventAt == "" {
		t.Fatalf("LastEventAt not set after success")
	}
	if got.LastError != "" {
		t.Fatalf("LastError = %q, want empty after success", got.LastError)
	}

	// A topic-gate rejection (400) records LastError.
	if res := h.post(t, src.ID, secret, `{"topic":"other","events":[{"data":{"n":1}}]}`); res.Status != http.StatusBadRequest {
		t.Fatalf("bad-topic POST: status = %d, want 400", res.Status)
	}
	got, err = h.svc.Get(ctx, scope, src.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastError == "" {
		t.Fatalf("LastError not set after a 400 rejection")
	}
}

// TestIngest_RouteMountSplit — the ingest route answers without an ApiKey
// (its own SourceKey auth), while /v1/feed/stream still 401s without one:
// proof the ingest mount lives OUTSIDE identity.Middleware.
func TestIngest_RouteMountSplit(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
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

	key := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, key)
	inbox := inboxapi.New(st)
	feed := feedapi.New(bus)
	svc := streamsource.New(st, key)
	srcIngest := feedingest.New(svc, bus)

	_, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	src, secret, err := svc.Create(ctx, scope, "relay", "orders")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	handler := httpapi.New(httpapi.Deps{Identity: id, Inbox: inbox, Feed: feed, SourceIngest: srcIngest})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Ingest works with SourceKey and NO ApiKey.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sources/"+src.ID+"/events",
		strings.NewReader(`{"events":[{"data":{"n":1}}]}`))
	req.Header.Set("Authorization", "SourceKey "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ingest Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest without ApiKey: status = %d, want 202", resp.StatusCode)
	}

	// The feed SSE route still 401s without an ApiKey (inside the auth group).
	fresp, err := http.Get(srv.URL + "/v1/feed/stream?topic=orders")
	if err != nil {
		t.Fatalf("feed Get: %v", err)
	}
	_ = fresp.Body.Close()
	if fresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("feed stream without ApiKey: status = %d, want 401", fresp.StatusCode)
	}
}
