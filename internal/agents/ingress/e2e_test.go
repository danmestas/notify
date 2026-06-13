package ingress_test

// ACI slice 1 proof tests (RFC 0002 §9). The shared sidecar harness and its
// helpers live in harness_test.go; the hardening proofs (plan 06 PR C) live
// in hardening_e2e_test.go.

import (
	"encoding/json"
	"fmt"
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

// TestTurnRetryDLQReplay_E2E is proof (c): with the stub agent dead (no
// subscriber), a composed turn fails and lands in the DLQ (transcript
// intact); after the stub returns, `dagnats dlq replay` re-executes the turn
// and the reply finally reaches the inbox — the conversation resumes, nothing
// dropped. The harness uses MaxAttempts=0 (fail-fast) so the DLQ appears
// without delay; see TestTurnRetryLadder_E2E for the retry-ladder proof.
func TestTurnRetryDLQReplay_E2E(t *testing.T) {
	h := newHarness(t)
	stub := h.startStub(t)
	frames := h.openSSE(t)

	// Warm-up turn proves the pipeline before the outage.
	first := h.compose(t, "", "warm up")
	waitFrame(t, frames, 20*time.Second, "ECHO::warm up")

	// Kill the stub: the next turn must dead-letter, not drop.
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

// TestTurnRetryLadder_E2E proves the retry ladder unlocked by
// danmestas/dagnats#384: with MaxAttempts=2 and a never-reply stub, the
// engine dispatches the turn MaxAttempts+1 times (initial + 2 retries = 3
// total) before dead-lettering. DLQ replay on a live stub resumes the
// conversation. Wall-clock budget: each round trip ≈ 7s (5s bridge poll +
// 2s mesh timeout); 3 rounds ≈ 21s, well under the 60s suite budget.
func TestTurnRetryLadder_E2E(t *testing.T) {
	h := newRetryHarness(t)
	stub := h.startStub(t)
	frames := h.openSSE(t)

	// Warm-up turn on the live stub proves the pipeline.
	first := h.compose(t, "", "ladder warm up")
	waitFrame(t, frames, 60*time.Second, "ECHO::ladder warm up") // fresh-engine first poll can be slow on loaded CI

	// Replace the stub with a never-reply variant to force repeated retries.
	if err := stub.Unsubscribe(); err != nil {
		t.Fatalf("stub unsubscribe: %v", err)
	}
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	dispatches := h.startNeverReplyStub(t, stubSubject)

	out := h.compose(t, first.ConversationID, "ladder payload")

	// The engine dispatches MaxAttempts+1 times total (initial + 2 retries).
	// Each round trip ≈ 7s (5s bridge poll + 2s mesh timeout + backoff);
	// 3 rounds ≈ 21s, so a 45s window covers all overhead.
	wantDispatches := int64(h.retry.MaxAttempts + 1)
	deadline := time.Now().Add(45 * time.Second)
	for dispatches.Load() < wantDispatches {
		if time.Now().After(deadline) {
			t.Fatalf("retry ladder stalled: got %d dispatches, want %d within 45s",
				dispatches.Load(), wantDispatches)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("retry ladder complete: %d dispatches observed", dispatches.Load())

	letters := h.waitDLQ(t, out.RunID, 30*time.Second)
	if len(letters) == 0 {
		t.Fatal("no DLQ entry after retry ladder")
	}

	// Replay on a live stub confirms the conversation resumes normally.
	h.startStub(t)
	h.dlqReplay(t, out.RunID)
	waitFrame(t, frames, 30*time.Second, "ECHO::", "ladder payload")
	msgs := h.waitMessages(t, 2, 10*time.Second)
	var found bool
	for _, m := range msgs {
		if strings.Contains(m.Content, "ladder payload") && m.RunID == out.RunID {
			found = true
		}
	}
	if !found {
		t.Errorf("no replayed reply row joining run %s: %+v", out.RunID, msgs)
	}
}

// TestTranscriptCursorPagination_E2E proves the before/after/limit query
// parameters on GET /v1/inbox/conversations/{agentIdentifier}:
//
//   - limit= caps the returned window
//   - before= fetches rows older than the given ULID
//   - after=  fetches rows newer than the given ULID
//
// The test fires two compose turns via the real pipeline and waits for both
// agent replies to appear in the in_app messages table (a stable store-poll
// signal that the turn handler has fully committed), then exercises cursor
// parameters on the transcript endpoint.
func TestTranscriptCursorPagination_E2E(t *testing.T) {
	h := newHarness(t)
	h.startStub(t)

	first := h.compose(t, "", "page turn 1")
	// Wait for the first reply to land in the messages table: the turn
	// handler fully commits before publishing the inbox event, so this is a
	// safe signal that the singleton is released before we issue turn 2.
	h.waitMessages(t, 1, 30*time.Second)

	second := h.compose(t, first.ConversationID, "page turn 2")
	// Two replies now expected.
	h.waitMessages(t, 2, 30*time.Second)

	// Full transcript (no cursor params): 4 activities (user, agent, user, agent).
	fullActs, _ := h.transcriptOf(t, h.agent.Identifier, first.ConversationID)
	if len(fullActs) != 4 {
		t.Fatalf("full transcript: want 4 activities, got %d: %+v", len(fullActs), fullActs)
	}

	// limit=2 (no cursor): most-recent 2 activities oldest-first.
	limited := getTranscript(t, h,
		fmt.Sprintf("?conversationId=%s&limit=2", first.ConversationID))
	if len(limited) != 2 {
		t.Fatalf("limit=2: want 2 activities, got %d", len(limited))
	}

	// before=<third activity id>: rows older than the third activity (index 2).
	// Expected: first two activities (user + agent from turn 1).
	thirdID := fullActs[2].ID
	beforePage := getTranscript(t, h,
		fmt.Sprintf("?conversationId=%s&before=%s&limit=10", first.ConversationID, thirdID))
	if len(beforePage) != 2 {
		t.Fatalf("before: want 2 activities, got %d: %+v", len(beforePage), beforePage)
	}
	if beforePage[0].ID != fullActs[0].ID || beforePage[1].ID != fullActs[1].ID {
		t.Errorf("before: ids=%v %v, want %v %v",
			beforePage[0].ID, beforePage[1].ID, fullActs[0].ID, fullActs[1].ID)
	}

	// after=<second activity id>: rows newer than the second activity (index 1).
	// Expected: activities at index 2 and 3.
	secondID := fullActs[1].ID
	afterPage := getTranscript(t, h,
		fmt.Sprintf("?conversationId=%s&after=%s&limit=10", first.ConversationID, secondID))
	if len(afterPage) != 2 {
		t.Fatalf("after: want 2 activities, got %d: %+v", len(afterPage), afterPage)
	}
	if afterPage[0].ID != fullActs[2].ID || afterPage[1].ID != fullActs[3].ID {
		t.Errorf("after: ids=%v %v, want %v %v",
			afterPage[0].ID, afterPage[1].ID, fullActs[2].ID, fullActs[3].ID)
	}

	// missing conversationId → 400.
	resp, err := http.Get(h.srv.URL + "/v1/inbox/conversations/" + h.agent.Identifier)
	if err != nil {
		t.Fatalf("GET no conversationId: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing conversationId: status=%d, want 400", resp.StatusCode)
	}

	_ = second // used to ensure two full turns completed
}

// getTranscript GETs the transcript with the given query suffix and returns the
// decoded activity slice.
func getTranscript(t *testing.T, h *harness, query string) []agentdomain.Activity {
	t.Helper()
	resp, err := http.Get(h.srv.URL + "/v1/inbox/conversations/" + h.agent.Identifier + query)
	if err != nil {
		t.Fatalf("GET transcript %s: %v", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript %s: status=%d, want 200", query, resp.StatusCode)
	}
	var tr struct {
		Activities []agentdomain.Activity `json:"activities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode transcript %s: %v", query, err)
	}
	return tr.Activities
}
