package ingress_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
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
	srv     *httptest.Server
	agent   agentdomain.Agent
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	eng, err := engine.New(natsURL, bridgeURL)
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

	// MaxAttempts=0: fail straight to the DLQ. In-engine retry attempts are
	// blocked on an upstream dagnats gap (the HTTP bridge never emits
	// step.started, so the attempt counter is pinned and retry #2's timer is
	// deduplicated; github.com/danmestas/dagnats/issues/381) — see the
	// matching note in ingress.New.
	ing := ingress.New(ags, st, eng, bus).
		WithRuntime(agentdomain.RuntimeMesh, meshClient).
		WithRetry(&engine.RetryPolicy{
			MaxAttempts:  0,
			Strategy:     "fixed",
			InitialDelay: 300 * time.Millisecond,
			MaxDelay:     300 * time.Millisecond,
		})
	eng.Handle(ingress.TaskMesh, ing.HandleTurn)
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

	return &harness{natsURL: natsURL, nc: nc, store: st, agents: ags, srv: srv, agent: agent}
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

type composeResponse struct {
	ConversationID string `json:"conversationId"`
	ActivityID     string `json:"activityId"`
	RunID          string `json:"runId"`
}

// compose POSTs one inbox message and decodes the 202 acknowledgement.
func (h *harness) compose(t *testing.T, conversationID, text string) composeResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"subscriberId":   subscriber,
		"conversationId": conversationID,
		"text":           text,
	})
	resp, err := http.Post(
		h.srv.URL+"/v1/inbox/conversations/"+h.agent.Identifier+"/messages",
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("compose POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read compose response: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("compose status = %d, want 202 (body: %s)", resp.StatusCode, raw)
	}
	var out composeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode compose response: %v (body: %s)", err, raw)
	}
	if out.ConversationID == "" || out.ActivityID == "" || out.RunID == "" {
		t.Fatalf("compose response incomplete: %+v", out)
	}
	return out
}

// openSSE attaches to the inbox stream and returns a channel of data frames.
func (h *harness) openSSE(t *testing.T) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.srv.URL+"/v1/inbox/stream?subscriberId="+subscriber, nil)
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
	deadline := time.Now().Add(timeout)
	for {
		msgs, err := h.store.Messages().List(context.Background(), testScope, subscriber, "in_app")
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

// TestInboxTurnLoop_E2E is proof (a) + (b) of ACI slice 1: a compose lands the
// agent's reply on the inbox SSE stream and as an in_app messages row; a
// second compose into the same conversation carries the bounded transcript,
// so the (echoing) stub's reply proves turn 1 was prepended into the prompt.
func TestInboxTurnLoop_E2E(t *testing.T) {
	h := newHarness(t)
	h.startStub(t)
	frames := h.openSSE(t)

	// (a) compose -> reply on the SSE stream and in the messages table.
	first := h.compose(t, "", "hello first turn")
	frame := waitFrame(t, frames, 20*time.Second, "ECHO::hello first turn")
	var fromStream domain.Message
	if err := json.Unmarshal([]byte(frame), &fromStream); err != nil {
		t.Fatalf("SSE frame is not a message JSON: %v (%s)", err, frame)
	}
	if fromStream.Channel != "in_app" || fromStream.SubscriberID != subscriber {
		t.Errorf("stream message = %+v, want in_app for %s", fromStream, subscriber)
	}
	msgs := h.waitMessages(t, 1, 5*time.Second)
	if msgs[0].Content != "ECHO::hello first turn" {
		t.Errorf("message content = %q, want echo of turn 1", msgs[0].Content)
	}
	if msgs[0].RunID != first.RunID {
		t.Errorf("message run id = %q, want %q (joins the dagnats run)", msgs[0].RunID, first.RunID)
	}

	// (b) second compose in the same conversation: the echoed prompt must
	// contain the prepended history with turn 1 (window N=20) and the new
	// message after the separator.
	second := h.compose(t, first.ConversationID, "second turn")
	if second.ConversationID != first.ConversationID {
		t.Fatalf("conversation = %q, want continuity with %q", second.ConversationID, first.ConversationID)
	}
	frame = waitFrame(t, frames, 20*time.Second,
		"Conversation so far:", "user: hello first turn", "agent: ECHO::hello first turn", "second turn")
	if strings.Contains(frame, "user: second turn") {
		t.Errorf("history must not contain the new message itself: %s", frame)
	}

	// Transcript read (RFC 0002 §7): full activity log, oldest first.
	resp, err := http.Get(h.srv.URL + "/v1/inbox/conversations/" + h.agent.Identifier +
		"?conversationId=" + first.ConversationID)
	if err != nil {
		t.Fatalf("transcript GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status = %d, want 200", resp.StatusCode)
	}
	var tr struct {
		Conversation agentdomain.Conversation `json:"conversation"`
		Activities   []agentdomain.Activity   `json:"activities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	roles := make([]string, 0, len(tr.Activities))
	for _, a := range tr.Activities {
		roles = append(roles, a.Role)
	}
	if got, want := strings.Join(roles, ","), "user,agent,user,agent"; got != want {
		t.Errorf("transcript roles = %s, want %s", got, want)
	}
	if tr.Conversation.SubscriberID != subscriber {
		t.Errorf("conversation subscriber = %q, want %q", tr.Conversation.SubscriberID, subscriber)
	}
}

// TestTurnRetryDLQReplay_E2E is proof (c): with the stub agent dead, a
// composed turn fails (retriable on the wire) and lands in the DLQ
// (transcript intact); after the stub returns, `dagnats dlq replay`
// re-executes the turn and the reply finally reaches the inbox — the
// conversation resumes, nothing dropped. Intermediate in-engine retry
// attempts are exercised the moment dagnats fixes bridge-worker attempt
// propagation (see ingress.New); the DLQ landing and replay are real here.
func TestTurnRetryDLQReplay_E2E(t *testing.T) {
	h := newHarness(t)
	stub := h.startStub(t)
	frames := h.openSSE(t)

	// Warm-up turn proves the pipeline before the outage.
	first := h.compose(t, "", "warm up")
	waitFrame(t, frames, 20*time.Second, "ECHO::warm up")

	// Kill the stub: the next turn must retry and dead-letter, not drop.
	if err := stub.Unsubscribe(); err != nil {
		t.Fatalf("stub unsubscribe: %v", err)
	}
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	out := h.compose(t, first.ConversationID, "while you were out")
	letters := h.waitDLQ(t, out.RunID, 60*time.Second)
	if len(letters) == 0 {
		t.Fatal("no DLQ entry for the failed turn run")
	}

	// Restart the stub and replay the dead letter: the reply arrives.
	h.startStub(t)
	h.dlqReplay(t, out.RunID)
	waitFrame(t, frames, 30*time.Second, "ECHO::", "while you were out")
	msgs := h.waitMessages(t, 2, 10*time.Second)
	var found bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "while you were out") && m.RunID == out.RunID {
			found = true
		}
	}
	if !found {
		t.Errorf("no replayed reply row joining run %s: %+v", out.RunID, msgs)
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

// dagnats runs the dagnats CLI against the test sidecar.
func (h *harness) dagnats(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(dagtest.Binary(t), args...)
	cmd.Env = append(os.Environ(), "DAGNATS_NATS_URL="+h.natsURL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("dagnats %s: %w (stderr: %s)", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}
