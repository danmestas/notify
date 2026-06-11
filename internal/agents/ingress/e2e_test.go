package ingress_test

// ACI slice 1 proof tests (RFC 0002 §9). The shared sidecar harness and its
// helpers live in harness_test.go; the hardening proofs (plan 06 PR C) live
// in hardening_e2e_test.go.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

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
