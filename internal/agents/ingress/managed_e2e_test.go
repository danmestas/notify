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

// postCompose POSTs a raw compose body and returns the status and body.
func (h *harness) postCompose(t *testing.T, agentIdentifier, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+"/v1/inbox/conversations/"+agentIdentifier+"/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestCompose_AttachmentsReachTheModelAndTheTranscript: attachments sent with
// a message are stored in its content, returned by the transcript, and shown
// to the model after the user's text, labelled with name and media type.
func TestCompose_AttachmentsReachTheModelAndTheTranscript(t *testing.T) {
	h := newHarness(t)
	srv, sent := fakeChatServer(t, "It totals 5.")
	cfg, _ := json.Marshal(map[string]any{"provider": "openai", "baseUrl": srv.URL + "/v1", "model": "m", "apiKey": "k"})
	agent, err := h.agents.Agents().Create(context.Background(), testScope, agentdomain.Agent{
		Identifier: "reader", Name: "Reader", Runtime: agentdomain.RuntimeManaged, Active: true, Managed: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}

	status, raw := h.postCompose(t, agent.Identifier, `{"subscriberId":"sub1","text":"Summarize","attachments":[{"name":"invoice.md","mediaType":"text/markdown","text":"# Invoice\nTotal: 5"}]}`)
	if status != http.StatusAccepted {
		t.Fatalf("compose: %d %s", status, raw)
	}
	var res composeResponse
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	convID := mustConvID(t, h, agent.ID, res.ConversationID)
	deadline := time.Now().Add(20 * time.Second)
	for {
		acts, err := h.agents.Activities().History(context.Background(), testScope, convID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(acts) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply; activities = %d", len(acts))
		}
		time.Sleep(100 * time.Millisecond)
	}

	_, body := sent()
	msgs, _ := json.Marshal(body["messages"])
	if !strings.Contains(string(msgs), `Summarize\n\nAttached: invoice.md (text/markdown)\n`+"```"+`\n# Invoice\nTotal: 5\n`+"```") {
		t.Fatalf("model messages = %s", msgs)
	}

	resp, err := http.Get(h.srv.URL + "/v1/inbox/conversations/reader?conversationId=" + res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tr struct {
		Activities []struct {
			Role    string `json:"role"`
			Content struct {
				Attachments []map[string]string `json:"attachments"`
				Sources     []map[string]any    `json:"sources"`
			} `json:"content"`
		} `json:"activities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	if len(tr.Activities) != 2 || len(tr.Activities[0].Content.Attachments) != 1 ||
		tr.Activities[0].Content.Attachments[0]["name"] != "invoice.md" ||
		tr.Activities[0].Content.Attachments[0]["mediaType"] != "text/markdown" ||
		tr.Activities[0].Content.Attachments[0]["text"] != "# Invoice\nTotal: 5" {
		t.Fatalf("transcript = %+v", tr)
	}
	if len(tr.Activities[1].Content.Attachments) != 0 || len(tr.Activities[0].Content.Sources) != 0 {
		t.Fatalf("attachments belong to the user message, sources to the reply: %+v", tr.Activities)
	}
	src := tr.Activities[1].Content.Sources
	if len(src) != 1 || src[0]["server"] != "" || src[0]["tool"] != "attachment" || src[0]["label"] != "invoice.md" {
		t.Fatalf("reply sources = %+v", src)
	}
}

// TestCompose_RejectsBadAttachments: attachments over the limits answer 422
// and start no turn.
func TestCompose_RejectsBadAttachments(t *testing.T) {
	h := newHarness(t)
	att := func(n int, name, text string) string {
		one, _ := json.Marshal(map[string]string{"name": name, "mediaType": "text/plain", "text": text})
		return "[" + strings.TrimSuffix(strings.Repeat(string(one)+",", n), ",") + "]"
	}
	cases := []struct{ name, attachments string }{
		{"nine attachments", att(9, "a", "x")},
		{"no name", att(1, "", "x")},
		{"long name", att(1, strings.Repeat("n", 201), "x")},
		{"text over 64 KB", att(1, "a", strings.Repeat("t", 64*1024+1))},
		{"over 256 KB in total", att(5, "a", strings.Repeat("t", 60*1024))},
	}
	// 5 x 60 KB = 300 KB: each within its own limit, over the total.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.postCompose(t, h.agent.Identifier, `{"subscriberId":"sub1","text":"hi","attachments":`+tc.attachments+`}`)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %.200s", status, body)
			}
		})
	}
}
