package feedingest_test

// Slice S6: end-to-end + adversarial PROOF for the stream-source webhook MVP.
// Exercises the full shipped ingest→feed→SSE stack — store + eventbus.Bus +
// streamsource + feedingest + feedapi mounted under the real httpapi.New with
// a real identity.Bootstrap tenant — against a live dagnats sidecar. Tests
// only: no production code lives in this file. A failing assertion that
// surfaces a genuine production bug loops back to the owning slice; this file
// never patches production.
//
// The SSE-client helpers (e2eFrame/e2eStream/openFeedStreamAt) are a deliberate
// copy of internal/httpapi/stream_e2e_test.go's unexported harness — a small,
// cross-package test-helper duplication the plan sanctions rather than
// extracting a shared testutil for three tiny funcs.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// masterKey mirrors the repo-wide test master-key pattern (32 bytes of 0x2a).
func masterKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0x2a
	}
	return k
}

// fullStack is the complete ingest→feed→SSE surface behind one httptest server:
// a real identity.Service (for ApiKey-authed SSE), a real streamsource.Service
// (for SourceKey-authed ingest), a feedapi.Gateway (the SSE bridge) and a
// feedingest.Gateway (the ingest endpoint), all sharing one eventbus.Bus over a
// live dagnats sidecar.
type fullStack struct {
	url string
	id  *identity.Service
	svc *streamsource.Service
	bus *eventbus.Bus
}

func newFullStack(t *testing.T) *fullStack {
	t.Helper()
	natsURL, _ := dagtest.StartSidecar(t)
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

	mk := masterKey()
	id := identity.New(st, mk)
	feed := feedapi.New(bus)
	svc := streamsource.New(st, mk)
	srcIngest := feedingest.New(svc, bus)
	inbox := inboxapi.New(st)

	// httpapi.New(id, aud, trg, inbox, rt, feed, inb, an, ag, ing, conn, st, ev,
	// eml, src, auth...). Only id + inbox + feed + src are load-bearing here; the
	// rest are nil (their routes are nil-guarded, except inbox.Routes which is
	// unconditional — hence the real inboxapi.New).
	handler := httpapi.New(id, nil, nil, inbox, nil, feed, nil, nil, nil, nil, nil, nil, nil, nil, srcIngest)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &fullStack{url: srv.URL, id: id, svc: svc, bus: bus}
}

// bootstrap provisions a fresh tenant and returns its ApiKey auth header +
// scope. Each call mints a distinct EnvID (a ULID), so two tenants are
// subject-disjoint on the shared NOTIFY_FEED stream.
func (fs *fullStack) bootstrap(t *testing.T, org, env string) (authHeader string, scope domain.Scope) {
	t.Helper()
	key, scope, err := fs.id.Bootstrap(context.Background(), org, env)
	if err != nil {
		t.Fatalf("Bootstrap(%s,%s): %v", org, env, err)
	}
	return "ApiKey " + key, scope
}

// ingest POSTs a batch to the ingest endpoint with a full Authorization header
// (SourceKey <hex> for producers; an ApiKey header when a test proves the
// scheme is rejected). It fully reads and closes the response.
func (fs *fullStack) ingest(t *testing.T, sourceID, authHeader, body string) ingResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fs.url+"/v1/sources/"+sourceID+"/events", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
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

// --- SSE client harness (copied from internal/httpapi/stream_e2e_test.go) -----

// e2eFrame is one parsed "id: <n>\ndata: <payload>\n\n" SSE frame.
type e2eFrame struct {
	id   string
	data string
}

// e2eStream wraps an open SSE connection with a single scanner goroutine feeding
// a channel, so a test can read frames incrementally without racing two
// scanners over the same body.
type e2eStream struct {
	resp   *http.Response
	frames chan e2eFrame
	cancel context.CancelFunc
}

// read blocks for n frames or fails the test after timeout.
func (s *e2eStream) read(t *testing.T, n int, timeout time.Duration) []e2eFrame {
	t.Helper()
	var got []e2eFrame
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
func (s *e2eStream) expectNoMore(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			t.Fatalf("received unexpected extra frame: %+v", f)
		}
	case <-time.After(d):
	}
}

func (s *e2eStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// openFeedStreamAt opens an authenticated GET /v1/feed/stream connection. A
// non-empty lastEventID sets the SSE Last-Event-ID resume header (the browser
// EventSource reconnect mechanism); feedapi prefers it over from= and resumes
// at lastEventID+1. Fatals if the response is not 200.
func openFeedStreamAt(t *testing.T, baseURL, authHeader, query, lastEventID string) *e2eStream {
	t.Helper()
	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/v1/feed/stream?"+query, nil)
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", authHeader)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by e2eStream.close()
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

	frames := make(chan e2eFrame, 256)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var cur e2eFrame
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
					cur = e2eFrame{}
				}
			}
		}
	}()

	return &e2eStream{resp: resp, frames: frames, cancel: cancel}
}

// decodeEnvelope decodes an SSE frame's data as the §0.4 ingest envelope.
func decodeEnvelope(t *testing.T, data string) env {
	t.Helper()
	var e env
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		t.Fatalf("decode frame as ingest envelope: %v (data=%s)", err, data)
	}
	return e
}

// --- S6.1 --------------------------------------------------------------------

// TestIngest_PostToSSE_RoundTrip is the headline round-trip: a SourceKey-authed
// batch POST lands on the durable feed and an ApiKey-authed SSE client reads it
// back in order — then a disconnect + reconnect with Last-Event-ID replays only
// the events published while disconnected (the durable replay-then-live promise
// proven over INGESTED data, not workflow sends).
func TestIngest_PostToSSE_RoundTrip(t *testing.T) {
	fs := newFullStack(t)
	authHeader, scope := fs.bootstrap(t, "Acme", "dev")
	src, secret := mustCreate(t, fs.svc, scope, "relay", "agent.tokens")
	sourceKey := "SourceKey " + secret

	stream := openFeedStreamAt(t, fs.url, authHeader, "topic=agent.tokens&from=0", "")
	t.Cleanup(stream.close)

	// POST a 5-event batch.
	body := `{"events":[{"data":{"n":1}},{"data":{"n":2}},{"data":{"n":3}},{"data":{"n":4}},{"data":{"n":5}}]}`
	if res := fs.ingest(t, src.ID, sourceKey, body); res.Status != http.StatusAccepted {
		t.Fatalf("batch POST: status = %d (%s), want 202", res.Status, res.Body)
	}

	frames := stream.read(t, 5, 15*time.Second)
	prevID := -1
	for i, f := range frames {
		id, err := strconv.Atoi(f.id)
		if err != nil {
			t.Fatalf("frame[%d] id %q not an integer: %v", i, f.id, err)
		}
		if id <= prevID {
			t.Fatalf("frame[%d] id = %d, not strictly increasing (prev=%d)", i, id, prevID)
		}
		prevID = id

		e := decodeEnvelope(t, f.data)
		if e.V != 1 {
			t.Fatalf("frame[%d] envelope v = %d, want 1", i, e.V)
		}
		if e.SourceID != src.ID {
			t.Fatalf("frame[%d] source_id = %q, want %q", i, e.SourceID, src.ID)
		}
		if e.EventID == "" || e.TS == "" {
			t.Fatalf("frame[%d] missing event_id/ts: %+v", i, e)
		}
		if n := dataN(t, e); n != i+1 {
			t.Fatalf("frame[%d] data.n = %d, want %d (order not preserved)", i, n, i+1)
		}
	}
	lastID := frames[4].id

	// Disconnect, publish 2 more, reconnect with Last-Event-ID → exactly the 2
	// new events, and nothing else.
	stream.close()

	more := `{"events":[{"data":{"n":6}},{"data":{"n":7}}]}`
	if res := fs.ingest(t, src.ID, sourceKey, more); res.Status != http.StatusAccepted {
		t.Fatalf("second POST: status = %d (%s), want 202", res.Status, res.Body)
	}

	resumed := openFeedStreamAt(t, fs.url, authHeader, "topic=agent.tokens", lastID)
	t.Cleanup(resumed.close)
	newFrames := resumed.read(t, 2, 15*time.Second)
	for i, f := range newFrames {
		e := decodeEnvelope(t, f.data)
		if n := dataN(t, e); n != i+6 {
			t.Fatalf("resumed frame[%d] data.n = %d, want %d", i, n, i+6)
		}
	}
	resumed.expectNoMore(t, 2*time.Second)
}

// --- S6.2 --------------------------------------------------------------------

// TestIngest_CrossTenantCannotReadOrWrite proves the tenant boundary from both
// sides: tenant B cannot READ tenant A's feed, and tenant A's SourceKey cannot
// WRITE outside its own env+topic prefix (nor can B's console key write at all).
func TestIngest_CrossTenantCannotReadOrWrite(t *testing.T) {
	fs := newFullStack(t)
	authA, scopeA := fs.bootstrap(t, "AcmeA", "dev")
	authB, scopeB := fs.bootstrap(t, "AcmeB", "dev")
	srcA, secretA := mustCreate(t, fs.svc, scopeA, "relay", "agent.tokens")
	sourceKeyA := "SourceKey " + secretA

	// (a) B's SSE on the identical topic string sees NONE of A's events. Open B
	// first so it is live before A publishes; A's own SSE confirms the events
	// really landed (so B's silence is isolation, not a lost publish).
	streamB := openFeedStreamAt(t, fs.url, authB, "topic=agent.tokens&from=0", "")
	t.Cleanup(streamB.close)

	if res := fs.ingest(t, srcA.ID, sourceKeyA, `{"events":[{"data":{"n":1}},{"data":{"n":2}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("A ingest: status = %d (%s), want 202", res.Status, res.Body)
	}
	streamA := openFeedStreamAt(t, fs.url, authA, "topic=agent.tokens&from=0", "")
	t.Cleanup(streamA.close)
	streamA.read(t, 2, 15*time.Second) // A sees its own 2 events
	streamB.expectNoMore(t, 3*time.Second)

	// (c) B's console ApiKey on the ingest endpoint → 401 (scheme mismatch: the
	// SourceKey scheme is required; a valid ApiKey is not a SourceKey).
	if res := fs.ingest(t, srcA.ID, authB, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusUnauthorized {
		t.Fatalf("B ApiKey on ingest: status = %d (%s), want 401", res.Status, res.Body)
	}

	// (d) A's SourceKey with crafted topics fails the prefix rule BEFORE any
	// subject is constructed. Load-bearing vector: a literal cross-tenant feed
	// subject as the topic value — it is a ValidateFeedTopic-legal string, so
	// only the `topic == src.Topic || HasPrefix(topic, src.Topic+".")` gate
	// stops it; plus one representative injection char. The exhaustive
	// charset/wildcard matrix is covered at the S4.3 unit layer
	// (TestIngest_TopicScoping) and not re-run here.
	crafted := []string{
		"notify.feed." + scopeB.EnvID + ".x", // cross-tenant WRITE attempt (valid topic, wrong prefix)
		">",                                  // representative injection (rejected by ValidateFeedTopic)
	}
	for _, bad := range crafted {
		reqBody := fmt.Sprintf(`{"topic":%q,"events":[{"data":{"n":9}}]}`, bad)
		if res := fs.ingest(t, srcA.ID, sourceKeyA, reqBody); res.Status != http.StatusBadRequest {
			t.Fatalf("crafted topic %q: status = %d (%s), want 400", bad, res.Status, res.Body)
		}
	}
	// No crafted write reached env B (its SSE has been live throughout).
	streamB.expectNoMore(t, 2*time.Second)

	// (b) After A's source is deleted, A's SourceKey no longer authenticates → 401.
	if err := fs.svc.Delete(context.Background(), scopeA, srcA.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res := fs.ingest(t, srcA.ID, sourceKeyA, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusUnauthorized {
		t.Fatalf("deleted-source ingest: status = %d (%s), want 401", res.Status, res.Body)
	}
}

// --- S6.3 --------------------------------------------------------------------

// TestIngest_FirehoseCannotEvictNeighbor is the retention-guard proof, framed as
// what the full stack can show EMPIRICALLY through the real HTTP pipeline:
//
//	(i)  a firehose source F is 429'd at its byte-budget boundary;
//	(ii) the total bytes NOTIFY_FEED accepts from F never exceeds its budget by
//	     more than one batch (the limiter fails closed BEFORE publish); and
//	(iii) a quiet neighbour Q in another env keeps ingesting AND replaying
//	     unaffected while/after F is throttled — proving the per-source limiter
//	     key + subject isolation end-to-end: a firehose cannot starve a neighbour.
//
// Note on scope: eviction-PREVENTION itself is the DOCUMENTED S3 retention
// arithmetic — budget × sources × MaxAge-window stays under NOTIFY_FEED's fixed
// 1 GiB MaxBytes (see internal/eventbus/budget.go) — not an empirical DiscardOld
// demonstration. Proving eviction empirically would require writing ~1 GiB to
// create eviction pressure against the fixed stream config; that is out of a
// fast test's budget, so this test proves the limiter boundary + neighbour
// isolation that the arithmetic rests on. (A stronger test-local tiny-MaxBytes
// DiscardOld stream could demonstrate eviction directly; deliberately not built
// here — the governed HTTP pipeline is what ships.)
func TestIngest_FirehoseCannotEvictNeighbor(t *testing.T) {
	fs := newFullStack(t)
	ctx := context.Background()

	authQ, scopeQ := fs.bootstrap(t, "Quiet", "dev")
	_, scopeF := fs.bootstrap(t, "Firehose", "dev")

	srcQ, secretQ := mustCreate(t, fs.svc, scopeQ, "quiet", "q.topic")
	srcF, secretF := mustCreate(t, fs.svc, scopeF, "firehose", "f.topic")
	sourceKeyQ := "SourceKey " + secretQ
	sourceKeyF := "SourceKey " + secretF

	// F: tiny 64 KiB byte budget (the binding limit); a generous event rate so
	// the byte budget — not the rate limit — is what trips.
	if err := fs.svc.UpdateLimits(ctx, scopeF, srcF.ID, 6000, 32768, 100, 64*1024); err != nil {
		t.Fatalf("UpdateLimits F: %v", err)
	}
	const fBudget int64 = 64 * 1024

	// Q publishes 3 events first and reads them back — its baseline replay window.
	if res := fs.ingest(t, srcQ.ID, sourceKeyQ, `{"events":[{"data":{"n":1}},{"data":{"n":2}},{"data":{"n":3}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("Q initial ingest: status = %d (%s), want 202", res.Status, res.Body)
	}
	streamQ := openFeedStreamAt(t, fs.url, authQ, "topic=q.topic&from=0", "")
	t.Cleanup(streamQ.close)
	streamQ.read(t, 3, 15*time.Second)

	// F firehoses fixed ~8 KiB/event batches until it is 429'd at its budget.
	const eventsPerBatch = 8
	pad := strings.Repeat("x", 1000) // each event's data ≈ 1010 bytes
	oneEvent := fmt.Sprintf(`{"data":{"pad":%q}}`, pad)
	perEventBytes := int64(len(fmt.Sprintf(`{"pad":%q}`, pad))) // matches handler's len(ev.Data)
	batchBody := `{"events":[` + strings.Repeat(oneEvent+",", eventsPerBatch-1) + oneEvent + `]}`
	batchDataBytes := perEventBytes * eventsPerBatch

	var acceptedDataBytes int64
	got429 := false
	for i := 0; i < 200 && !got429; i++ {
		res := fs.ingest(t, srcF.ID, sourceKeyF, batchBody)
		switch res.Status {
		case http.StatusAccepted:
			acceptedDataBytes += batchDataBytes
		case http.StatusTooManyRequests:
			got429 = true
			if res.Header.Get("Retry-After") == "" {
				t.Fatalf("F 429 missing Retry-After header")
			}
		default:
			t.Fatalf("F firehose batch %d: status = %d (%s), want 202 or 429", i, res.Status, res.Body)
		}
	}

	// (i) F was throttled at its boundary.
	if !got429 {
		t.Fatalf("F was never 429'd within 200 batches; budget guard not enforced")
	}
	// (ii) NOTIFY_FEED accepted no more than budget + one batch from F.
	if acceptedDataBytes > fBudget+batchDataBytes {
		t.Fatalf("F accepted %d data bytes, want <= budget(%d) + one batch(%d)", acceptedDataBytes, fBudget, batchDataBytes)
	}
	if acceptedDataBytes == 0 {
		t.Fatalf("F accepted 0 bytes; the firehose never got past the limiter at all")
	}

	// (iii) Q is unaffected AFTER F is throttled: it can still ingest live...
	if res := fs.ingest(t, srcQ.ID, sourceKeyQ, `{"events":[{"data":{"n":4}},{"data":{"n":5}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("Q post-firehose ingest: status = %d (%s), want 202", res.Status, res.Body)
	}
	streamQ.read(t, 2, 15*time.Second) // the 2 new events arrive live on the still-open stream

	// ...and its full replay window survived — a fresh from=0 SSE replays all 5.
	replay := openFeedStreamAt(t, fs.url, authQ, "topic=q.topic&from=0", "")
	t.Cleanup(replay.close)
	all := replay.read(t, 5, 15*time.Second)
	for i, f := range all {
		if n := dataN(t, decodeEnvelope(t, f.data)); n != i+1 {
			t.Fatalf("Q replay frame[%d] data.n = %d, want %d (replay window not intact)", i, n, i+1)
		}
	}
	replay.expectNoMore(t, 2*time.Second)
}

// --- S6.4 --------------------------------------------------------------------

// TestIngest_MixedShapesOneBridge proves the two-shapes-per-topic contract: a
// workflow-produced domain.Message ("channel":"stream") and an ingested §0.4
// envelope ("v":1) coexist on one env+topic and reach a single SSE consumer,
// which distinguishes them by their discriminator keys. The workflow event is
// published directly via bus.PublishFeed(env, topic, domain.Message JSON) — the
// lighter mechanism the plan permits over standing up a delivery.NewStream
// harness, proving the same two shapes with less scaffolding.
func TestIngest_MixedShapesOneBridge(t *testing.T) {
	fs := newFullStack(t)
	authHeader, scope := fs.bootstrap(t, "Acme", "dev")
	src, secret := mustCreate(t, fs.svc, scope, "relay", "mixed")

	stream := openFeedStreamAt(t, fs.url, authHeader, "topic=mixed&from=0", "")
	t.Cleanup(stream.close)

	// Shape 1: a workflow send.stream message — domain.Message JSON.
	msg := domain.Message{
		ID:      store.NewULID(),
		OrgID:   scope.OrgID,
		EnvID:   scope.EnvID,
		Channel: "stream",
		Subject: "mixed",
		Content: "hello from a workflow",
		Status:  "sent",
		Extras:  json.RawMessage("{}"),
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal domain.Message: %v", err)
	}
	if err := fs.bus.PublishFeed(scope.EnvID, "mixed", msgJSON); err != nil {
		t.Fatalf("PublishFeed workflow message: %v", err)
	}

	// Shape 2: an ingested envelope via the real HTTP ingest endpoint.
	if res := fs.ingest(t, src.ID, "SourceKey "+secret, `{"events":[{"data":{"n":1}}]}`); res.Status != http.StatusAccepted {
		t.Fatalf("ingest: status = %d (%s), want 202", res.Status, res.Body)
	}

	frames := stream.read(t, 2, 15*time.Second)

	var sawWorkflow, sawIngested bool
	for i, f := range frames {
		var probe struct {
			V       *int   `json:"v"`
			Channel string `json:"channel"`
		}
		if err := json.Unmarshal([]byte(f.data), &probe); err != nil {
			t.Fatalf("frame[%d] not JSON: %v (data=%s)", i, err, f.data)
		}
		switch {
		case probe.Channel == "stream":
			if probe.V != nil {
				t.Fatalf("frame[%d] has both channel:stream AND v — shapes not distinct: %s", i, f.data)
			}
			m := decodeMixedMessage(t, f.data)
			if m.Content != "hello from a workflow" {
				t.Fatalf("frame[%d] workflow content = %q, want %q", i, m.Content, "hello from a workflow")
			}
			sawWorkflow = true
		case probe.V != nil:
			if *probe.V != 1 {
				t.Fatalf("frame[%d] ingested envelope v = %d, want 1", i, *probe.V)
			}
			e := decodeEnvelope(t, f.data)
			if e.SourceID != src.ID {
				t.Fatalf("frame[%d] ingested source_id = %q, want %q", i, e.SourceID, src.ID)
			}
			sawIngested = true
		default:
			t.Fatalf("frame[%d] carries neither discriminator: %s", i, f.data)
		}
	}
	if !sawWorkflow || !sawIngested {
		t.Fatalf("both shapes must reach the one bridge: sawWorkflow=%v sawIngested=%v", sawWorkflow, sawIngested)
	}
}

// decodeMixedMessage decodes an SSE frame's data as the persisted domain.Message
// a workflow stream send carries.
func decodeMixedMessage(t *testing.T, data string) domain.Message {
	t.Helper()
	var m domain.Message
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("decode frame as domain.Message: %v (data=%s)", err, data)
	}
	return m
}
