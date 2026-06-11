package ingress_test

// Hardening proofs for the ACI turn loop (plan 06, PR C): concurrent-
// conversation isolation, server-restart-mid-turn replay, and webhook
// redelivery dedup. Same conventions as e2e_test.go — real dagnats sidecar,
// temp SQLite, pi-style stub agents, no mocks, -race. The shared harness
// lives in harness_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/engine"
)

const stubSubject2 = "agents.prompt.test.stub2"

// startCountingStub subscribes a pi-style A2A echo agent on subject that
// counts every dispatch it receives and answers "<prefix><prompt>" on the
// reply_to inbox from a separate goroutine (with a small stagger so
// concurrent turns genuinely overlap). With holdFirst, dispatch #1 is acked
// but never answered — the turn hangs between dispatch and reply, which is
// exactly the window the restart proof interrupts.
func (h *harness) startCountingStub(t *testing.T, subject, prefix string, holdFirst bool) (count *atomic.Int64, received <-chan string) {
	t.Helper()
	var n atomic.Int64
	rcv := make(chan string, 16)
	_, err := h.nc.Subscribe(subject, func(m *nats.Msg) {
		var env struct {
			Parts []agentdomain.Part `json:"parts"`
		}
		_ = json.Unmarshal(m.Data, &env)
		_ = m.Respond([]byte(`{}`))
		var text string
		for _, p := range env.Parts {
			text += p.Text
		}
		seq := n.Add(1)
		select {
		case rcv <- text:
		default:
		}
		if holdFirst && seq == 1 {
			return // acked, never answers: the turn is stuck mid-flight
		}
		replyTo := m.Header.Get("reply_to")
		go func() {
			time.Sleep(50 * time.Millisecond)
			answer, _ := json.Marshal(map[string]any{
				"messageId": "stub-answer",
				"role":      "agent",
				"parts":     []map[string]string{{"text": prefix + text}},
			})
			_ = h.nc.Publish(replyTo, answer)
		}()
	})
	if err != nil {
		t.Fatalf("counting stub subscribe: %v", err)
	}
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("counting stub flush: %v", err)
	}
	return &n, rcv
}

// assertDispatchesHold proves the absence of further dispatches: it polls
// count every 100ms (the waitMessagesFor idiom — no sleep-as-synchronization)
// for the whole window, failing the moment the counter exceeds want, and
// requires exact equality once the window closes.
func assertDispatchesHold(t *testing.T, count *atomic.Int64, want int64, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got := count.Load(); got > want {
			t.Fatalf("dispatches = %d, want %d to hold for %s", got, want, window)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := count.Load(); got != want {
		t.Fatalf("dispatches = %d after %s window, want exactly %d", got, window, want)
	}
}

// turnInputJSON rebuilds the engine run input the compose handler produces —
// the wire contract of RFC 0002 §5 (idempotency key dot-path "activity_id",
// singleton key dot-path "conversation_id") — so tests can simulate a webhook
// redelivery: a second StartRun carrying the same activity id.
func turnInputJSON(t *testing.T, agentIdentifier, conversationID, activityID string) json.RawMessage {
	t.Helper()
	in, err := json.Marshal(map[string]string{
		"org_id":           testScope.OrgID,
		"env_id":           testScope.EnvID,
		"agent_identifier": agentIdentifier,
		"conversation_id":  conversationID,
		"activity_id":      activityID,
	})
	if err != nil {
		t.Fatalf("marshal turn input: %v", err)
	}
	return in
}

// conversationRowID resolves the inbox thread handle (platform_thread_id,
// what compose returns) to the conversation's internal row id (what the turn
// input carries).
func (h *harness) conversationRowID(t *testing.T, agentID, threadID string) string {
	t.Helper()
	conv, err := h.agents.Conversations().GetByThread(context.Background(), testScope, agentID, "", threadID)
	if err != nil {
		t.Fatalf("GetByThread(%s): %v", threadID, err)
	}
	return conv.ID
}

// transcriptOf GETs the conversation transcript and returns the decoded
// activities plus the raw body (for substring contamination checks).
func (h *harness) transcriptOf(t *testing.T, agentIdentifier, threadID string) ([]agentdomain.Activity, []byte) {
	t.Helper()
	resp, err := http.Get(h.srv.URL + "/v1/inbox/conversations/" + agentIdentifier +
		"?conversationId=" + threadID)
	if err != nil {
		t.Fatalf("transcript GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript status = %d, want 200 (body: %s)", resp.StatusCode, raw)
	}
	var tr struct {
		Activities []agentdomain.Activity `json:"activities"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	return tr.Activities, raw
}

// activityRoles joins the activity roles oldest-first ("user,agent").
func activityRoles(acts []agentdomain.Activity) string {
	roles := make([]string, 0, len(acts))
	for _, a := range acts {
		roles = append(roles, a.Role)
	}
	return strings.Join(roles, ",")
}

// TestConcurrentConversationIsolation_E2E is hardening proof (1): six
// conversations across two mesh agents, every turn in flight simultaneously,
// each carrying a unique marker. Every reply must land in its own
// conversation, echoed by its own agent, with its own marker — no
// cross-contamination of replies, transcripts, or the per-subscriber SSE
// fan-out. (Pins today's safe envelope: pi-style reply_to inboxes only;
// claude-style stream correlation is ACI-4.)
func TestConcurrentConversationIsolation_E2E(t *testing.T) {
	const conversations = 6
	h := newHarness(t)

	agentB, err := h.agents.Agents().Create(context.Background(), testScope, agentdomain.Agent{
		Identifier:  "stub2",
		Name:        "Stub Two",
		Runtime:     agentdomain.RuntimeMesh,
		Active:      true,
		MeshSubject: stubSubject2,
	})
	if err != nil {
		t.Fatalf("create second agent: %v", err)
	}
	countA, _ := h.startCountingStub(t, stubSubject, "ECHO-A::", false)
	countB, _ := h.startCountingStub(t, stubSubject2, "ECHO-B::", false)

	marker := func(i int) string { return fmt.Sprintf("isolation-marker-%d", i) }
	subOf := func(i int) string { return fmt.Sprintf("iso-sub-%d", i) }
	textOf := func(i int) string { return "please reply with " + marker(i) }
	identOf := func(i int) string {
		if i%2 == 1 {
			return agentB.Identifier
		}
		return h.agent.Identifier
	}
	prefixOf := func(i int) string {
		if i%2 == 1 {
			return "ECHO-B::"
		}
		return "ECHO-A::"
	}

	// One SSE stream on conversation 0's subscriber: it must see exactly its
	// own reply and nothing from the other five conversations.
	frames := h.openSSEFor(t, subOf(0))

	// Fire all six composes simultaneously so all six turns are in flight at
	// once (engine workers run each task in its own goroutine; the stubs
	// answer asynchronously with a stagger).
	results := make([]composeResponse, conversations)
	errs := make([]error, conversations)
	var wg sync.WaitGroup
	for i := range conversations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = h.composeErr(identOf(i), subOf(i), "", textOf(i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("compose %d: %v", i, err)
		}
	}

	// Every reply lands with its own marker, its own agent's prefix, and its
	// own run id; no other conversation's marker appears anywhere near it.
	for i := range conversations {
		msgs := h.waitMessagesFor(t, subOf(i), 1, 30*time.Second)
		if len(msgs) != 1 {
			t.Fatalf("conversation %d: messages rows = %d, want exactly 1: %+v", i, len(msgs), msgs)
		}
		if want := prefixOf(i) + textOf(i); msgs[0].Content != want {
			t.Errorf("conversation %d: reply = %q, want %q", i, msgs[0].Content, want)
		}
		if msgs[0].RunID != results[i].RunID {
			t.Errorf("conversation %d: reply run id = %q, want %q", i, msgs[0].RunID, results[i].RunID)
		}

		acts, raw := h.transcriptOf(t, identOf(i), results[i].ConversationID)
		if got := activityRoles(acts); got != "user,agent" {
			t.Errorf("conversation %d: transcript roles = %s, want user,agent", i, got)
		}
		if !strings.Contains(string(raw), marker(i)) {
			t.Errorf("conversation %d: transcript missing its own marker %s", i, marker(i))
		}
		for j := range conversations {
			if j != i && strings.Contains(string(raw), marker(j)) {
				t.Errorf("conversation %d: transcript contaminated with %s: %s", i, marker(j), raw)
			}
		}
	}

	// Exactly one dispatch per conversation reached each agent (3 + 3).
	if got := countA.Load(); got != conversations/2 {
		t.Errorf("agent A dispatches = %d, want %d", got, conversations/2)
	}
	if got := countB.Load(); got != conversations/2 {
		t.Errorf("agent B dispatches = %d, want %d", got, conversations/2)
	}

	// The SSE stream for subscriber 0 carries exactly its own reply: first
	// the expected frame, then silence (any further frame is a duplicate or
	// another conversation's fan-out leaking across subscribers). The
	// transcript absence-of-foreign-markers loop above is the authoritative
	// cross-contamination assertion; this drain is supplementary.
	waitFrame(t, frames, 20*time.Second, marker(0))
	drain := time.After(3 * time.Second)
	for {
		select {
		case f := <-frames:
			t.Errorf("unexpected extra SSE frame on %s's stream: %s", subOf(0), f)
		case <-drain:
			return
		}
	}
}

// TestRestartMidTurnIdempotentReplay_E2E is hardening proof (2): the engine
// worker dies between dispatching a turn to the agent and receiving the
// reply. The turn dead-letters (transcript intact) rather than vanishing; a
// restarted worker re-submitting the same inbound activity (StartRun with
// the same idempotency key) gets the existing run back — no duplicate user
// activity, no duplicate turn — and a DLQ replay completes the conversation
// with exactly one reply.
func TestRestartMidTurnIdempotentReplay_E2E(t *testing.T) {
	h := newHarness(t)
	count, received := h.startCountingStub(t, stubSubject, "ECHO::", true)
	frames := h.openSSE(t)

	const text = "restart-proof marker"
	out := h.compose(t, "", text)

	// Wait until the dispatch has reached the agent, then kill the worker —
	// the turn is now interrupted between dispatch and reply. Stop cancels
	// the in-flight handler (the dispatch fails retriable) and drains it.
	select {
	case <-received:
	case <-time.After(30 * time.Second):
		t.Fatal("dispatch never reached the stub")
	}
	if err := h.eng.Stop(); err != nil {
		t.Fatalf("engine stop: %v", err)
	}

	// The interrupted turn dead-letters; nothing was persisted for it.
	h.waitDLQ(t, out.RunID, 60*time.Second)
	if msgs, err := h.store.Messages().List(context.Background(), testScope, subscriber, "in_app"); err != nil || len(msgs) != 0 {
		t.Fatalf("messages rows after interrupt = %d (err %v), want 0", len(msgs), err)
	}

	// "Restart the server": a fresh engine + ingress worker on the same
	// sidecar, stores, and bus.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng2, err := engine.New(h.natsURL, h.bridgeURL)
	if err != nil {
		t.Fatalf("engine.New (restart): %v", err)
	}
	t.Cleanup(func() { _ = eng2.Stop() })
	ing2 := ingress.New(h.agents, h.store, eng2, h.bus).
		WithRuntime(agentdomain.RuntimeMesh, h.mesh).
		WithRetry(h.retry)
	eng2.Handle(ingress.TaskMesh, ing2.HandleTurn)
	eng2.StartWorkers(ctx)

	// Re-submitting the inbound activity across the restart (what a webhook
	// redelivery or a recovering server would do) must return the existing
	// run, not start a duplicate turn.
	convID := h.conversationRowID(t, h.agent.ID, out.ConversationID)
	input := turnInputJSON(t, h.agent.Identifier, convID, out.ActivityID)
	rid, err := eng2.StartRun(ctx, "agent-turn-"+h.agent.ID, input)
	if err != nil {
		t.Fatalf("StartRun across restart: %v", err)
	}
	if rid != out.RunID {
		t.Errorf("StartRun across restart minted run %s, want existing %s (idempotency key broken)", rid, out.RunID)
	}
	// The idempotent re-submit must dispatch nothing: the counter has to
	// hold at exactly 1 (the interrupted dispatch) for the whole window.
	assertDispatchesHold(t, count, 1, 3*time.Second)

	// Replay the dead letter: the turn re-executes (the stub answers
	// dispatches after the first) and the reply lands exactly once.
	h.dlqReplay(t, out.RunID)
	waitFrame(t, frames, 30*time.Second, "ECHO::", text)

	h.waitMessagesFor(t, subscriber, 1, 10*time.Second)
	time.Sleep(1 * time.Second) // settle: catch any late duplicate write
	msgs, err := h.store.Messages().List(context.Background(), testScope, subscriber, "in_app")
	if err != nil {
		t.Fatalf("Messages().List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages rows after replay = %d, want exactly 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Content != "ECHO::"+text || msgs[0].RunID != out.RunID {
		t.Errorf("replayed reply = %+v, want ECHO::%s joining run %s", msgs[0], text, out.RunID)
	}

	// One user activity (compose ran once; the re-submit added nothing) and
	// one agent reply — the turn completed exactly once end to end.
	acts, _ := h.transcriptOf(t, h.agent.Identifier, out.ConversationID)
	if got := activityRoles(acts); got != "user,agent" {
		t.Errorf("transcript roles = %s, want user,agent (no duplicate activity)", got)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("total dispatches = %d, want 2 (interrupted + replay)", got)
	}
}

// TestRedeliveryDedup_E2E is hardening proof (3): the same turn started twice
// with the same activity id — a simulated webhook redelivery, both while the
// first run is in flight and again after it completed — yields the same run
// id both times and exactly one dispatch to the agent, one reply, one
// transcript entry pair.
func TestRedeliveryDedup_E2E(t *testing.T) {
	h := newHarness(t)
	count, _ := h.startCountingStub(t, stubSubject, "ECHO::", false)
	frames := h.openSSE(t)

	const text = "redelivery-proof marker"
	out := h.compose(t, "", text)

	// Redeliver immediately (the first run is typically still in flight).
	convID := h.conversationRowID(t, h.agent.ID, out.ConversationID)
	input := turnInputJSON(t, h.agent.Identifier, convID, out.ActivityID)
	ctx := context.Background()
	rid, err := h.eng.StartRun(ctx, "agent-turn-"+h.agent.ID, input)
	if err != nil {
		t.Fatalf("redelivered StartRun: %v", err)
	}
	if rid != out.RunID {
		t.Errorf("redelivered StartRun minted run %s, want existing %s (idempotency key broken)", rid, out.RunID)
	}

	waitFrame(t, frames, 20*time.Second, "ECHO::"+text)

	// Redeliver again after completion — the late-redelivery case.
	rid2, err := h.eng.StartRun(ctx, "agent-turn-"+h.agent.ID, input)
	if err != nil {
		t.Fatalf("post-completion StartRun: %v", err)
	}
	if rid2 != out.RunID {
		t.Errorf("post-completion StartRun minted run %s, want existing %s", rid2, out.RunID)
	}

	// Settle window: the two no-op redeliveries must not dispatch — the
	// counter has to hold at exactly 1 until the deadline.
	assertDispatchesHold(t, count, 1, 3*time.Second)
	msgs, err := h.store.Messages().List(ctx, testScope, subscriber, "in_app")
	if err != nil {
		t.Fatalf("Messages().List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages rows = %d, want exactly 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Content != "ECHO::"+text || msgs[0].RunID != out.RunID {
		t.Errorf("reply = %+v, want ECHO::%s joining run %s", msgs[0], text, out.RunID)
	}
	acts, _ := h.transcriptOf(t, h.agent.Identifier, out.ConversationID)
	if got := activityRoles(acts); got != "user,agent" {
		t.Errorf("transcript roles = %s, want user,agent (no duplicate turn)", got)
	}
}
