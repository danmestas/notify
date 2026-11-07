package ingress_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
)

// retryTurn POSTs the retry route and returns the status and body.
func (h *harness) retryTurn(t *testing.T, agentIdentifier, conversationID, activityID string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+"/v1/agents/"+agentIdentifier+"/conversations/"+
		conversationID+"/activities/"+activityID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// waitTaskState polls the turn's status record until it reaches want.
func (h *harness) waitTaskState(t *testing.T, activityID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rec, ok := h.tasks.Get(context.Background(), testScope, activityID)
		if ok && rec.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s = %+v (found %v), want state %s within %s", activityID, rec, ok, want, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// failedTurn composes a message with no agent listening and waits for its turn
// to exhaust its retries.
func (h *harness) failedTurn(t *testing.T) composeResponse {
	t.Helper()
	out := h.compose(t, "", "retry me")
	h.waitTaskState(t, out.ActivityID, agentdomain.TaskFailed, 60*time.Second)
	return out
}

func TestRetryFailedTurn_E2E(t *testing.T) {
	h := newRetryHarness(t)
	// A definition left by the previous binary keys idempotency on activity_id.
	// Registering the current one must replace it, or the retry's new key
	// would be ignored and the old failed run handed back.
	if err := h.eng.RegisterWorkflow(context.Background(), engine.WorkflowDef{
		Name:           names.AgentTurnWorkflow(h.agent.ID),
		IdempotencyKey: "activity_id",
		Steps:          []engine.StepDef{{ID: "turn", Task: names.AgentTurn(agentdomain.RuntimeMesh), Type: dag.StepTypeNormal}},
	}); err != nil {
		t.Fatalf("register old definition: %v", err)
	}
	frames := h.openSSE(t)
	out := h.failedTurn(t)

	h.startStub(t)
	status, body := h.retryTurn(t, h.agent.Identifier, out.ConversationID, out.ActivityID)
	if status != http.StatusAccepted {
		t.Fatalf("retry status = %d (%s), want 202", status, body)
	}
	var got composeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if got.RunID == "" || got.RunID == out.RunID {
		t.Errorf("retry run = %q, want a new run (first was %q)", got.RunID, out.RunID)
	}
	if got.ConversationID != out.ConversationID || got.ActivityID != out.ActivityID {
		t.Errorf("retry response = %+v, want conversation and activity of %+v", got, out)
	}

	waitFrame(t, frames, 30*time.Second, "ECHO::retry me")
	h.waitTaskState(t, out.ActivityID, agentdomain.TaskCompleted, 10*time.Second)
	acts, _ := h.transcriptOf(t, h.agent.Identifier, out.ConversationID)
	if r := activityRoles(acts); r != "user,agent" {
		t.Errorf("transcript roles = %s, want user,agent (one reply on the same conversation)", r)
	}
	rec, _ := h.tasks.Get(context.Background(), testScope, out.ActivityID)
	var retried bool
	for _, n := range rec.Notes {
		retried = retried || n.Text == "Retried by operator"
	}
	if !retried {
		t.Errorf("task notes = %+v, want a %q note", rec.Notes, "Retried by operator")
	}

	// Now answered: a second retry is refused.
	if status, body := h.retryTurn(t, h.agent.Identifier, out.ConversationID, out.ActivityID); status != http.StatusConflict {
		t.Errorf("retry of answered activity = %d (%s), want 409", status, body)
	}
}

func TestRetryRefusals_E2E(t *testing.T) {
	h := newRetryHarness(t)
	h.startStub(t)
	frames := h.openSSE(t)
	out := h.compose(t, "", "hello")
	waitFrame(t, frames, 60*time.Second, "ECHO::hello")

	acts, _ := h.transcriptOf(t, h.agent.Identifier, out.ConversationID)
	var agentActID string
	for _, a := range acts {
		if a.Role == "agent" {
			agentActID = a.ID
		}
	}
	if agentActID == "" {
		t.Fatal("no agent activity in transcript")
	}

	other := h.compose(t, "", "elsewhere")
	waitFrame(t, frames, 30*time.Second, "ECHO::elsewhere")

	tests := []struct {
		name                          string
		agent, conversation, activity string
		want                          int
	}{
		{"answered user activity", h.agent.Identifier, out.ConversationID, out.ActivityID, http.StatusConflict},
		{"agent activity", h.agent.Identifier, out.ConversationID, agentActID, http.StatusConflict},
		{"unknown agent", "nope", out.ConversationID, out.ActivityID, http.StatusNotFound},
		{"unknown conversation", h.agent.Identifier, "nope", out.ActivityID, http.StatusNotFound},
		{"unknown activity", h.agent.Identifier, out.ConversationID, "nope", http.StatusNotFound},
		{"activity of another conversation", h.agent.Identifier, out.ConversationID, other.ActivityID, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := h.retryTurn(t, tc.agent, tc.conversation, tc.activity); status != tc.want {
				t.Errorf("status = %d (%s), want %d", status, body, tc.want)
			}
		})
	}
}

func TestRetryWhileRunning_E2E(t *testing.T) {
	h := newRetryHarness(t)
	out := h.failedTurn(t)

	// An agent that acks but never answers keeps the retried turn running.
	h.startNeverReplyStub(t, stubSubject)
	if status, body := h.retryTurn(t, h.agent.Identifier, out.ConversationID, out.ActivityID); status != http.StatusAccepted {
		t.Fatalf("first retry = %d (%s), want 202", status, body)
	}
	status, body := h.retryTurn(t, h.agent.Identifier, out.ConversationID, out.ActivityID)
	if status != http.StatusConflict || !strings.Contains(string(body), "running") {
		t.Errorf("second retry = %d (%s), want 409 running", status, body)
	}
}

// A retry of a turn that has not failed is refused: it may still be running.
func TestRetryOfOpenTurn_E2E(t *testing.T) {
	h := newRetryHarness(t)
	h.startNeverReplyStub(t, stubSubject)
	out := h.compose(t, "", "slow")
	if status, body := h.retryTurn(t, h.agent.Identifier, out.ConversationID, out.ActivityID); status != http.StatusConflict {
		t.Errorf("retry of running turn = %d (%s), want 409", status, body)
	}
}

// A retry is refused for an activity with no turn status: it may have been
// answered or be running, and a second run could double-reply.
func TestRetryWithoutTurnStatus_E2E(t *testing.T) {
	h := newRetryHarness(t)
	out := h.compose(t, "", "seed")
	h.waitTaskState(t, out.ActivityID, agentdomain.TaskFailed, 60*time.Second)
	convID := h.conversationRowID(t, h.agent.ID, out.ConversationID)
	legacy, err := h.agents.Activities().Append(context.Background(), testScope, agentdomain.Activity{
		ConversationID: convID, Role: "user", Content: json.RawMessage(`{"parts":[{"text":"legacy"}]}`),
	})
	if err != nil {
		t.Fatalf("append legacy activity: %v", err)
	}
	if status, body := h.retryTurn(t, h.agent.Identifier, out.ConversationID, legacy.ID); status != http.StatusConflict {
		t.Errorf("retry without status = %d (%s), want 409", status, body)
	}
}

// A retry started while another run of the conversation holds the turn lock
// is not admitted by the engine. It must be refused and leave the turn failed
// (so it can be retried), not stay "submitted" behind a run that never runs.
func TestRetryWhileConversationBusy_E2E(t *testing.T) {
	h := newRetryHarness(t)
	failed := h.failedTurn(t)

	// A newer message in the same conversation holds the lock while its
	// agent acks but never answers.
	dispatches := h.startNeverReplyStub(t, stubSubject)
	held := h.compose(t, failed.ConversationID, "holds the lock")
	deadline := time.Now().Add(30 * time.Second)
	for dispatches.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("holding turn never dispatched")
		}
		time.Sleep(50 * time.Millisecond)
	}

	status, body := h.retryTurn(t, h.agent.Identifier, failed.ConversationID, failed.ActivityID)
	if status != http.StatusConflict || !strings.Contains(string(body), "busy") {
		t.Fatalf("retry while busy = %d (%s), want 409 busy", status, body)
	}
	if rec, _ := h.tasks.Get(context.Background(), testScope, failed.ActivityID); rec.State != agentdomain.TaskFailed {
		t.Fatalf("task after skipped retry = %q, want failed", rec.State)
	}

	// Once the holder ends, the same turn can be retried.
	h.waitTaskState(t, held.ActivityID, agentdomain.TaskFailed, 60*time.Second)
	h.startStub(t)
	deadline = time.Now().Add(30 * time.Second)
	for {
		status, body = h.retryTurn(t, h.agent.Identifier, failed.ConversationID, failed.ActivityID)
		if status == http.StatusAccepted {
			break
		}
		if status != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("retry after holder ended = %d (%s), want 202", status, body)
		}
		time.Sleep(200 * time.Millisecond) // the engine may not have released the lock yet
	}
	h.waitTaskState(t, failed.ActivityID, agentdomain.TaskCompleted, 30*time.Second)
}
