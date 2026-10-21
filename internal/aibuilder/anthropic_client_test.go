package aibuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestAnthropicClientMessage_ParsesUsage asserts anthropicClient.Message
// populates MessageResponse.Usage from the Messages API's usage.input_tokens /
// usage.output_tokens fields — the token accounting AgentLoopBuilder's token
// budget relies on. anthropicClient is otherwise "never exercised by the unit
// tests" (see client.go); this test points the SDK at a local httptest server
// via option.WithBaseURL rather than adding network-dependent coverage.
func TestAnthropicClientMessage_ParsesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-test",
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  42,
				"output_tokens": 17,
			},
		})
	}))
	defer srv.Close()

	c := &anthropicClient{sdk: anthropic.NewClient(
		option.WithAPIKey("sk-test"),
		option.WithBaseURL(srv.URL),
	)}

	resp, err := c.Message(context.Background(), MessageRequest{
		Model:     "claude-test",
		Messages:  []Message{{Role: RoleUser, Text: "hi"}},
		MaxTokens: DefaultMaxTokens,
	})
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if resp.Usage.InputTokens != 42 {
		t.Errorf("Usage.InputTokens = %d, want 42", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 17 {
		t.Errorf("Usage.OutputTokens = %d, want 17", resp.Usage.OutputTokens)
	}
	if resp.Text != "hello" {
		t.Errorf("Text = %q, want hello", resp.Text)
	}
}

// TestAnthropicClientMessage_ToolChoiceNone: "none" keeps the tools defined
// (a transcript holding tool_use blocks needs them) but forbids new calls.
func TestAnthropicClientMessage_ToolChoiceNone(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-test",
			"content":     []any{map[string]any{"type": "text", "text": "done"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()
	c := &anthropicClient{sdk: anthropic.NewClient(option.WithAPIKey("sk-test"), option.WithBaseURL(srv.URL))}
	_, err := c.Message(context.Background(), MessageRequest{
		Model:      "claude-test",
		Messages:   []Message{{Role: RoleUser, Text: "hi"}},
		Tools:      []ToolDef{{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := body["tool_choice"].(map[string]any)
	if tc["type"] != "none" {
		t.Fatalf("tool_choice = %v, want {\"type\":\"none\"}", body["tool_choice"])
	}
	if tools, _ := body["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools = %v, want the one tool kept", body["tools"])
	}
}
