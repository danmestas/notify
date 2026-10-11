package ingress_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// composeKeyed POSTs one message with an Idempotency-Key header and returns
// the status and decoded body.
func (h *harness) composeKeyed(t *testing.T, key, conversationID, text string) (int, composeResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"subscriberId": subscriber, "conversationId": conversationID, "text": text})
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/inbox/conversations/"+h.agent.Identifier+"/messages", bytes.NewReader(body))
	if err != nil {
		t.Errorf("request: %v", err)
		return 0, composeResponse{}
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("compose POST: %v", err)
		return 0, composeResponse{}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out composeResponse
	if resp.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Errorf("decode: %v (%s)", err, raw)
		}
	}
	return resp.StatusCode, out
}

// userActivities counts the user rows in a conversation's transcript.
func (h *harness) userActivities(t *testing.T, conversationID string) int {
	t.Helper()
	resp, err := http.Get(h.srv.URL + "/v1/inbox/conversations/" + h.agent.Identifier + "?conversationId=" + conversationID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tr struct {
		Activities []struct {
			Role string `json:"Role"`
		} `json:"activities"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("decode transcript: %v (%s)", err, raw)
	}
	n := 0
	for _, a := range tr.Activities {
		if a.Role == "user" {
			n++
		}
	}
	return n
}

// TestCompose_IdempotencyKeyReplays is issue 3: a retry of the same message
// with the same key returns the original result and appends nothing; this
// holds for the opening message of a new thread too, where the client has no
// conversation id yet.
func TestCompose_IdempotencyKeyReplays(t *testing.T) {
	h := newHarness(t)

	status, first := h.composeKeyed(t, "msg-1", "", "open the thread")
	if status != http.StatusAccepted || first.RunID == "" {
		t.Fatalf("first compose: %d %+v", status, first)
	}
	status, again := h.composeKeyed(t, "msg-1", "", "open the thread")
	if status != http.StatusAccepted || again != first {
		t.Fatalf("retry of the opening message = %d %+v, want 202 %+v", status, again, first)
	}

	status, second := h.composeKeyed(t, "msg-2", first.ConversationID, "follow up")
	if status != http.StatusAccepted {
		t.Fatalf("second compose: %d", status)
	}
	status, replay := h.composeKeyed(t, "msg-2", first.ConversationID, "follow up")
	if status != http.StatusAccepted || replay != second {
		t.Fatalf("replay = %d %+v, want 202 %+v", status, replay, second)
	}
	if n := h.userActivities(t, first.ConversationID); n != 2 {
		t.Fatalf("user activities = %d, want 2 (one per distinct message)", n)
	}
}

// TestCompose_ConcurrentSameKeyYieldsOneMessage: simultaneous sends with one
// key produce one activity and one run, and none fails.
func TestCompose_ConcurrentSameKeyYieldsOneMessage(t *testing.T) {
	h := newHarness(t)
	_, seed := h.composeKeyed(t, "seed", "", "seed")

	var wg sync.WaitGroup
	results := make([]composeResponse, 4)
	statuses := make([]int, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], results[i] = h.composeKeyed(t, "race-1", seed.ConversationID, "at the same time")
		}(i)
	}
	wg.Wait()
	for i := range results {
		if statuses[i] != http.StatusAccepted || results[i] != results[0] {
			t.Fatalf("send %d = %d %+v, want 202 %+v", i, statuses[i], results[i], results[0])
		}
	}
	if n := h.userActivities(t, seed.ConversationID); n != 2 {
		t.Fatalf("user activities = %d, want 2 (seed + one concurrent message)", n)
	}
}

// TestCompose_KeyRulesAndScope covers the rest of the contract.
func TestCompose_KeyRulesAndScope(t *testing.T) {
	h := newHarness(t)
	_, a := h.composeKeyed(t, "k", "", "first text")
	_, b := h.composeKeyed(t, "seed-b", "", "other thread")

	if status, _ := h.composeKeyed(t, "k", a.ConversationID, "different text"); status != http.StatusConflict {
		t.Fatalf("same key, different text: %d, want 409", status)
	}
	if status, other := h.composeKeyed(t, "k", b.ConversationID, "first text"); status != http.StatusAccepted || other.ActivityID == a.ActivityID {
		t.Fatalf("same key in another conversation: %d %+v, want a separate activity", status, other)
	}
	for _, bad := range []string{"has space", strings.Repeat("y", 65), "semi;colon"} {
		if status, _ := h.composeKeyed(t, bad, a.ConversationID, "first text"); status != http.StatusUnprocessableEntity {
			t.Fatalf("invalid key %q: %d, want 422", bad, status)
		}
	}
	before := h.userActivities(t, a.ConversationID)
	for i := 0; i < 2; i++ {
		if status, _ := h.composeKeyed(t, "", a.ConversationID, "same text, no key"); status != http.StatusAccepted {
			t.Fatalf("keyless compose: %d", status)
		}
	}
	if n := h.userActivities(t, a.ConversationID); n != before+2 {
		t.Fatalf("keyless composes appended %d, want 2 (unchanged behaviour)", n-before)
	}
}
