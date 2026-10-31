package ingress_test

// Shared e2e harness for the ingress turn-loop tests: a real dagnats sidecar
// (dagtest), temp SQLite, eventbus, mesh client, and the HTTP surface
// (ingress compose + transcript, realtime SSE) behind a scope-injecting
// middleware. Extracted verbatim from e2e_test.go so the hardening proofs
// (hardening_e2e_test.go) can reuse it; the only additions are the harness
// fields exposing the engine/bus/mesh wiring (needed to restart the worker
// mid-test) and parameterized variants of compose/openSSE/waitMessages.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/dagnats/cli"
	"github.com/danmestas/dagnats/dagnatstest"
	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/agents/agentstep"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/agents/runtime/managed"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/store"
)

const (
	stubSubject = "agents.prompt.test.stub"
	subscriber  = "sub1"
)

var testScope = domain.Scope{OrgID: "org_1", EnvID: "env_1"}

// harness is the full turn-loop stack against a real sidecar: engine workers,
// temp SQLite, eventbus, mesh client, and the HTTP surface (ingress compose +
// transcript, realtime SSE) behind a scope-injecting middleware.
type harness struct {
	natsURL string
	nc      *nats.Conn
	store   *store.Store
	agents  *agentstore.Store
	bus     *eventbus.Bus
	mesh    *mesh.Client
	eng     *engine.Engine
	retry   *engine.RetryPolicy
	srv     *httptest.Server
	agent   agentdomain.Agent
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	natsURL := dagtest.StartSidecar(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ags := agentstore.New(st)

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	meshClient, err := mesh.Dial(natsURL, mesh.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	t.Cleanup(meshClient.Close)

	// MaxAttempts=0 in the harness: fail straight to DLQ so the suite stays
	// fast. Production uses MaxAttempts=3 (see ingress.New); individual tests
	// that need to exercise the retry ladder override via WithRetry explicitly
	// (danmestas/dagnats#384 unlocked MaxAttempts>0 — see ingress.New comment).
	retry := &engine.RetryPolicy{
		MaxAttempts:  0,
		Strategy:     dag.RetryFixed,
		InitialDelay: 300 * time.Millisecond,
		MaxDelay:     300 * time.Millisecond,
	}
	ing := ingress.New(ags, st, eng, bus).
		WithRuntime(agentdomain.RuntimeMesh, meshClient).
		WithRuntime(agentdomain.RuntimeManaged, managed.New(nil, managed.DefaultClient)).
		WithRetry(retry)
	eng.Handle(ingress.TaskMesh, ing.HandleTurn)
	eng.Handle(ingress.TaskManaged, ing.HandleTurn)
	eng.Handle("agent_message", agentstep.New(st, ing, ags.Drafts()).Handle)
	eng.StartWorkers(ctx)

	agent, err := ags.Agents().Create(ctx, testScope, agentdomain.Agent{
		Identifier:  "stub",
		Name:        "Stub",
		Runtime:     agentdomain.RuntimeMesh,
		Active:      true,
		MeshSubject: stubSubject,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	r := chi.NewRouter()
	ing.Routes(r)
	realtime.New(bus).Routes(r)
	srv := httptest.NewServer(scopeInjector(testScope, r))
	t.Cleanup(srv.Close)

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	return &harness{
		natsURL: natsURL,
		nc:      nc,
		store:   st,
		agents:  ags,
		bus:     bus,
		mesh:    meshClient,
		eng:     eng,
		retry:   retry,
		srv:     srv,
		agent:   agent,
	}
}

// newRetryHarness returns a harness identical to newHarness except the retry
// policy uses MaxAttempts=3 with a short InitialDelay (50ms), exercising the
// full retry ladder in bounded wall time. Used by TestTurnRetryLadder_E2E.
func newRetryHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	// Stop the base harness engine so it does not compete for task dispatch
	// during the retry ladder test. The retry harness brings up its own engine
	// and ingress with MaxAttempts=3 on the same sidecar and stores.
	if err := h.eng.Stop(); err != nil {
		t.Fatalf("newRetryHarness stop base engine: %v", err)
	}

	// MaxAttempts=2: 1 initial + 2 retries = 3 total dispatches. Each round
	// trip is about 2s (the mesh timeout), well under the 45s window in
	// TestTurnRetryLadder_E2E.
	ladderRetry := &engine.RetryPolicy{
		MaxAttempts:  2,
		Strategy:     dag.RetryFixed,
		InitialDelay: 50 * time.Millisecond,
		MaxDelay:     50 * time.Millisecond,
	}

	meshClient, err := mesh.Dial(h.natsURL, mesh.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("newRetryHarness mesh.Dial: %v", err)
	}
	t.Cleanup(meshClient.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng2, err := engine.New(h.natsURL)
	if err != nil {
		t.Fatalf("newRetryHarness engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng2.Stop() })

	ing2 := ingress.New(h.agents, h.store, eng2, h.bus).
		WithRuntime(agentdomain.RuntimeMesh, meshClient).
		WithRetry(ladderRetry)
	eng2.Handle(ingress.TaskMesh, ing2.HandleTurn)
	eng2.StartWorkers(ctx)

	r := chi.NewRouter()
	ing2.Routes(r)
	realtime.New(h.bus).Routes(r)
	srv2 := httptest.NewServer(scopeInjector(testScope, r))
	t.Cleanup(srv2.Close)

	h2 := *h // shallow copy; override the wired fields
	h2.eng = eng2
	h2.mesh = meshClient
	h2.retry = ladderRetry
	h2.srv = srv2
	return &h2
}

// scopeInjector mimics identity.Middleware by attaching a fixed Scope, so the
// e2e exercises the real handlers without provisioning API keys.
func scopeInjector(scope domain.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(domain.ContextWithScope(r.Context(), scope)))
	})
}

// startStub subscribes a deterministic pi-style A2A echo agent: ack the
// request, then publish "ECHO::<prompt>" as the answer envelope to the
// reply_to inbox. Returns the subscription so tests can kill it.
func (h *harness) startStub(t *testing.T) *nats.Subscription {
	t.Helper()
	sub, err := h.nc.Subscribe(stubSubject, func(m *nats.Msg) {
		var env struct {
			Parts []agentdomain.Part `json:"parts"`
		}
		_ = json.Unmarshal(m.Data, &env)
		_ = m.Respond([]byte(`{}`))
		var text string
		for _, p := range env.Parts {
			text += p.Text
		}
		answer, _ := json.Marshal(map[string]any{
			"messageId": "stub-answer",
			"role":      "agent",
			"parts":     []map[string]string{{"text": "ECHO::" + text}},
		})
		_ = h.nc.Publish(m.Header.Get("reply_to"), answer)
	})
	if err != nil {
		t.Fatalf("stub subscribe: %v", err)
	}
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("stub flush: %v", err)
	}
	return sub
}

// startNeverReplyStub subscribes on subject, acks every dispatch, and counts
// it — but never publishes an answer. The mesh client times out, the engine
// sees a retriable failure, and the step is retried per the RetryPolicy. This
// is the harness side of the retry-ladder proof in TestTurnRetryLadder_E2E.
func (h *harness) startNeverReplyStub(t *testing.T, subject string) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	_, err := h.nc.Subscribe(subject, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{}`)) // ack the request; never answer the turn
		n.Add(1)
	})
	if err != nil {
		t.Fatalf("never-reply stub subscribe: %v", err)
	}
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("never-reply stub flush: %v", err)
	}
	return &n
}

type composeResponse struct {
	ConversationID string `json:"conversationId"`
	ActivityID     string `json:"activityId"`
	RunID          string `json:"runId"`
}

// compose POSTs one inbox message and decodes the 202 acknowledgement.
func (h *harness) compose(t *testing.T, conversationID, text string) composeResponse {
	t.Helper()
	return h.composeWith(t, h.agent.Identifier, subscriber, conversationID, text)
}

// composeWith is compose parameterized over the target agent and subscriber.
func (h *harness) composeWith(t *testing.T, agentIdentifier, subscriberID, conversationID, text string) composeResponse {
	t.Helper()
	out, err := h.composeErr(agentIdentifier, subscriberID, conversationID, text)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// composeErr is the error-returning core of compose, safe to call from
// non-test goroutines (the concurrency proof fires composes in parallel).
func (h *harness) composeErr(agentIdentifier, subscriberID, conversationID, text string) (composeResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"subscriberId":   subscriberID,
		"conversationId": conversationID,
		"text":           text,
	})
	resp, err := http.Post(
		h.srv.URL+"/v1/inbox/conversations/"+agentIdentifier+"/messages",
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return composeResponse{}, fmt.Errorf("compose POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return composeResponse{}, fmt.Errorf("read compose response: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return composeResponse{}, fmt.Errorf("compose status = %d, want 202 (body: %s)", resp.StatusCode, raw)
	}
	var out composeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return composeResponse{}, fmt.Errorf("decode compose response: %w (body: %s)", err, raw)
	}
	if out.ConversationID == "" || out.ActivityID == "" || out.RunID == "" {
		return composeResponse{}, fmt.Errorf("compose response incomplete: %+v", out)
	}
	return out, nil
}

// openSSE attaches to the inbox stream and returns a channel of data frames.
func (h *harness) openSSE(t *testing.T) <-chan string {
	t.Helper()
	return h.openSSEFor(t, subscriber)
}

// openSSEFor is openSSE parameterized over the subscriber.
func (h *harness) openSSEFor(t *testing.T, subscriberID string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.srv.URL+"/v1/inbox/stream?subscriberId="+subscriberID, nil)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader goroutine below
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
	}
	frames := make(chan string, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				select {
				case frames <- strings.TrimPrefix(line, "data: "):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return frames
}

// waitFrame blocks until a frame containing every want substring arrives.
func waitFrame(t *testing.T, frames <-chan string, timeout time.Duration, want ...string) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f := <-frames:
			ok := true
			for _, w := range want {
				if !strings.Contains(f, w) {
					ok = false
					break
				}
			}
			if ok {
				return f
			}
		case <-deadline:
			t.Fatalf("no SSE frame containing %q within %s", want, timeout)
			return ""
		}
	}
}

// waitMessages polls the in_app feed until want rows exist.
func (h *harness) waitMessages(t *testing.T, want int, timeout time.Duration) []domain.Message {
	t.Helper()
	return h.waitMessagesFor(t, subscriber, want, timeout)
}

// waitMessagesFor is waitMessages parameterized over the subscriber.
func (h *harness) waitMessagesFor(t *testing.T, subscriberID string, want int, timeout time.Duration) []domain.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msgs, err := h.store.Messages().List(context.Background(), testScope, subscriberID, "in_app")
		if err != nil {
			t.Fatalf("Messages().List: %v", err)
		}
		if len(msgs) >= want {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("messages rows = %d, want %d within %s", len(msgs), want, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitDLQ polls `dagnats dlq list --json --run=<runID>` until an entry shows.
func (h *harness) waitDLQ(t *testing.T, runID string, timeout time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		stdout, err := h.dagnats(t, "dlq", "list", "--json", "--run="+runID)
		if err == nil {
			var letters []map[string]any
			if jsonErr := json.Unmarshal(stdout, &letters); jsonErr == nil && len(letters) > 0 {
				return letters
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no DLQ entry for run %s within %s (last err: %v)", runID, timeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dlqReplay replays every dead letter of the run via the dagnats CLI.
func (h *harness) dlqReplay(t *testing.T, runID string) {
	t.Helper()
	if _, err := h.dagnats(t, "dlq", "replay", "--run="+runID, "--json"); err != nil {
		t.Fatalf("dlq replay: %v", err)
	}
}

// dagnats runs the dagnats CLI in-process against the test sidecar.
func (h *harness) dagnats(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	cliFix := dagnatstest.NewCLIFixture(&dagnatstest.Harness{NC: h.nc}, cli.Run, cli.SwapExitFunc)
	out, err := cliFix.RunErr(t, args...)
	if err != nil {
		return []byte(out), fmt.Errorf("dagnats %s: %w", strings.Join(args, " "), err)
	}
	return []byte(out), nil
}
