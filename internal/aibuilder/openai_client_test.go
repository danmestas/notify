package aibuilder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// chatCompletionFixture returns the minimal valid JSON shape for an OpenAI
// Chat Completions response. toolCalls may be nil.
func chatCompletionFixture(finishReason, content string, toolCalls []map[string]any) map[string]any {
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
		"refusal": "",
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1_700_000_000,
		"model":   "test-model",
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
				"logprobs":      nil,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}
}

// openAITestServer creates an httptest server that captures the raw request body,
// responds with the JSON from respFn, and closes on test cleanup.
func openAITestServer(t *testing.T, captured *[]byte, respFn func() map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		*captured = body
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(respFn()); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
}

// TestOpenAIClientMessage_WireShape asserts the openaiClient translates a
// MessageRequest carrying a system prompt, tools, a prior assistant tool_call,
// and a tool result into the correct OpenAI Chat Completions wire format. It
// verifies literal field names and values the API requires for the tool_call_id
// round-trip (messages[2].tool_calls[0].id → messages[3].tool_call_id).
func TestOpenAIClientMessage_WireShape(t *testing.T) {
	var captured []byte
	srv := openAITestServer(t, &captured, func() map[string]any {
		return chatCompletionFixture("stop", "", nil)
	})
	defer srv.Close()

	c := NewOpenAIClient("sk-test", srv.URL, "test-model")

	req := MessageRequest{
		Model:  "test-model",
		System: "You are a builder.",
		Messages: []Message{
			{Role: RoleUser, Text: "build a workflow"},
			{
				Role: RoleAssistant,
				Text: "I will add a step.",
				ToolCalls: []ToolCall{
					{ID: "call_abc", Name: "addStep", Input: json.RawMessage(`{"type":"email"}`)},
				},
			},
			{
				Role: RoleUser,
				ToolResults: []ToolResult{
					{ToolCallID: "call_abc", Content: "step added"},
				},
			},
		},
		Tools: []ToolDef{
			{
				Name:        "addStep",
				Description: "Add a step",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string"}}}`),
			},
		},
		ToolChoice: "auto",
		MaxTokens:  DefaultMaxTokens,
	}

	if _, err := c.Message(context.Background(), req); err != nil {
		t.Fatalf("Message: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 4 {
		t.Fatalf("messages: got %v (len %d), want 4 items", body["messages"], len(msgs))
	}

	// messages[0] — system
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("messages[0].role = %q, want system", sys["role"])
	}
	if sys["content"] != "You are a builder." {
		t.Errorf("messages[0].content = %q, want %q", sys["content"], "You are a builder.")
	}

	// messages[1] — user text
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("messages[1].role = %q, want user", user["role"])
	}
	if user["content"] != "build a workflow" {
		t.Errorf("messages[1].content = %q, want %q", user["content"], "build a workflow")
	}

	// messages[2] — assistant with tool_call
	asst, _ := msgs[2].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("messages[2].role = %q, want assistant", asst["role"])
	}
	asstTCs, _ := asst["tool_calls"].([]any)
	if len(asstTCs) != 1 {
		t.Fatalf("messages[2].tool_calls len = %d, want 1", len(asstTCs))
	}
	tc0, _ := asstTCs[0].(map[string]any)
	if tc0["id"] != "call_abc" {
		t.Errorf("tool_calls[0].id = %q, want call_abc", tc0["id"])
	}
	fn0, _ := tc0["function"].(map[string]any)
	if fn0["name"] != "addStep" {
		t.Errorf("tool_calls[0].function.name = %q, want addStep", fn0["name"])
	}
	if fn0["arguments"] != `{"type":"email"}` {
		t.Errorf("tool_calls[0].function.arguments = %q, want {\"type\":\"email\"}", fn0["arguments"])
	}

	// messages[3] — tool result; tool_call_id must echo the assistant's id
	tool, _ := msgs[3].(map[string]any)
	if tool["role"] != "tool" {
		t.Errorf("messages[3].role = %q, want tool", tool["role"])
	}
	if tool["tool_call_id"] != "call_abc" {
		t.Errorf("messages[3].tool_call_id = %q, want call_abc (round-trip broken)", tool["tool_call_id"])
	}
	if tool["content"] != "step added" {
		t.Errorf("messages[3].content = %q, want step added", tool["content"])
	}

	// tools array
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	t0, _ := tools[0].(map[string]any)
	fn, _ := t0["function"].(map[string]any)
	if fn["name"] != "addStep" {
		t.Errorf("tools[0].function.name = %q, want addStep", fn["name"])
	}
	if fn["description"] != "Add a step" {
		t.Errorf("tools[0].function.description = %q, want Add a step", fn["description"])
	}

	// tool_choice
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", body["tool_choice"])
	}
}

// TestOpenAIClientMessage_ParsesToolCallsResponse asserts the response parser
// converts a tool_calls finish_reason + tool_calls array into MessageResponse
// with the correct ToolCalls (id, name, input JSON) and StopReason.
func TestOpenAIClientMessage_ParsesToolCallsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		resp := chatCompletionFixture("tool_calls", "", []map[string]any{
			{
				"id":   "call_xyz",
				"type": "function",
				"function": map[string]any{
					"name":      "addStep",
					"arguments": `{"type":"email"}`,
				},
			},
		})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewOpenAIClient("sk-test", srv.URL, "test-model")
	resp, err := c.Message(context.Background(), MessageRequest{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Text: "build"}},
	})
	if err != nil {
		t.Fatalf("Message: %v", err)
	}

	if resp.StopReason != "tool_calls" {
		t.Errorf("StopReason = %q, want tool_calls", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_xyz" {
		t.Errorf("ToolCall.ID = %q, want call_xyz", resp.ToolCalls[0].ID)
	}
	if resp.ToolCalls[0].Name != "addStep" {
		t.Errorf("ToolCall.Name = %q, want addStep", resp.ToolCalls[0].Name)
	}
	if string(resp.ToolCalls[0].Input) != `{"type":"email"}` {
		t.Errorf("ToolCall.Input = %q, want {\"type\":\"email\"}", resp.ToolCalls[0].Input)
	}
	if resp.Text != "" {
		t.Errorf("Text = %q, want empty", resp.Text)
	}
}

// TestOpenAIClientMessage_ParsesTextResponse asserts the response parser
// converts a plain text (stop) response into MessageResponse.Text with an
// empty ToolCalls slice.
func TestOpenAIClientMessage_ParsesTextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		resp := chatCompletionFixture("stop", "Here is your workflow.", nil)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewOpenAIClient("sk-test", srv.URL, "test-model")
	resp, err := c.Message(context.Background(), MessageRequest{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Text: "build"}},
	})
	if err != nil {
		t.Fatalf("Message: %v", err)
	}

	if resp.Text != "Here is your workflow." {
		t.Errorf("Text = %q, want Here is your workflow.", resp.Text)
	}
	if resp.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", resp.StopReason)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want empty", resp.ToolCalls)
	}
}
