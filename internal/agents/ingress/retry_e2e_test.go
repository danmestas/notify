package ingress_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
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
