package ingress_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
)

// fakeChatServer is an OpenAI-compatible chat endpoint that answers every
// request with reply and records what it was sent.
func fakeChatServer(t *testing.T, reply string) (*httptest.Server, func() (auth string, body map[string]any)) {
	t.Helper()
	var mu sync.Mutex
	var auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		_ = json.Unmarshal(raw, &body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": reply},
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, map[string]any) { mu.Lock(); defer mu.Unlock(); return auth, body }
}

// TestManagedAgent_AnswersThroughItsOwnModel: a managed agent's turn calls
// the OpenAI-compatible endpoint its owner configured, with the agent's model,
// system prompt and key, and the answer lands in the transcript.
func TestManagedAgent_AnswersThroughItsOwnModel(t *testing.T) {
	h := newHarness(t)
	srv, sent := fakeChatServer(t, "Friday at noon works.")
	cfg, _ := json.Marshal(map[string]any{
		"provider": "openai", "baseUrl": srv.URL + "/v1", "model": "local-llama",
		"systemPrompt": "You draft short email replies.", "apiKey": "k-123",
	})
	agent, err := h.agents.Agents().Create(context.Background(), testScope, agentdomain.Agent{
		Identifier: "drafter", Name: "Drafter", Runtime: agentdomain.RuntimeManaged, Active: true, Managed: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}

	res := h.composeWith(t, agent.Identifier, subscriber, "", "Lunch Friday?")
	deadline := time.Now().Add(20 * time.Second)
	for {
		acts, err := h.agents.Activities().History(context.Background(), testScope, mustConvID(t, h, agent.ID, res.ConversationID), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(acts) == 2 && acts[1].Role == "agent" {
			if !strings.Contains(string(acts[1].Content), "Friday at noon works.") {
				t.Fatalf("agent reply = %s", acts[1].Content)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no managed reply; activities = %d", len(acts))
		}
		time.Sleep(100 * time.Millisecond)
	}
	auth, body := sent()
	if auth != "Bearer k-123" || body["model"] != "local-llama" {
		t.Fatalf("model call auth=%q model=%v", auth, body["model"])
	}
	msgs, _ := json.Marshal(body["messages"])
	if !strings.Contains(string(msgs), "You draft short email replies.") || !strings.Contains(string(msgs), "Lunch Friday?") {
		t.Fatalf("messages = %s", msgs)
	}
}

func mustConvID(t *testing.T, h *harness, agentID, thread string) string {
	t.Helper()
	conv, err := h.agents.Conversations().GetByThread(context.Background(), testScope, agentID, "", thread)
	if err != nil {
		t.Fatalf("conversation %q: %v", thread, err)
	}
	return conv.ID
}
