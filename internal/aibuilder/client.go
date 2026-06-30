// Package aibuilder is the engine core of notify's AI workflow builder. It
// mirrors the reference iterative mutation-tool design: a language model drives a
// conversation, calling typed tools (addStep, editStepContent, ...) that mutate
// an in-memory WorkflowDraft one step at a time, until it stops calling tools
// and the draft is validated by compiling it through the pure authoring path.
//
// The package is deliberately free of UI, HTTP, and persistence concerns (those
// are later phases). It depends on a small Client interface so the loop can be
// unit-tested with a scripted mock and no API key.
package aibuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is the Claude model the builder drives by default. It is a
// configurable field on the Builder; this const is the seed value.
const DefaultModel = "claude-sonnet-4-6"

// DefaultMaxTokens caps the assistant's per-turn output. The Anthropic Messages
// API requires max_tokens; 4096 is comfortably above a single batch of tool
// calls plus a final summary.
const DefaultMaxTokens = 4096

// Role identifies the author of a conversation Message.
type Role string

const (
	// RoleUser is the human / tool-result side of the conversation.
	RoleUser Role = "user"
	// RoleAssistant is the model side of the conversation.
	RoleAssistant Role = "assistant"
)

// ToolDef is a provider-agnostic tool definition: a name, a natural-language
// description, and a JSON-schema object describing the tool's input shape.
type ToolDef struct {
	Name        string
	Description string
	// InputSchema is a full JSON Schema object,
	// e.g. {"type":"object","properties":{...},"required":[...]}.
	InputSchema json.RawMessage
}

// ToolCall is one tool invocation the model requested in an assistant turn.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is the outcome of running a ToolCall, fed back to the model on the
// next user turn. IsError marks a failed application so the model can correct.
type ToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// Message is one conversation turn. An assistant turn carries Text and/or
// ToolCalls; a user turn carries Text and/or ToolResults.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// MessageRequest is one call to the model.
type MessageRequest struct {
	Model      string
	System     string
	Messages   []Message
	Tools      []ToolDef
	ToolChoice string // "auto" (default), reserved: "any", "none"
	MaxTokens  int
}

// MessageResponse is the model's reply: free text, any tool calls, and the
// reason the turn stopped ("end_turn", "tool_use", ...).
type MessageResponse struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
}

// Client abstracts the LLM so the builder loop is testable with a mock. The
// real implementation talks to the Anthropic Messages API.
type Client interface {
	Message(ctx context.Context, req MessageRequest) (MessageResponse, error)
}

// anthropicClient is the production Client backed by the official Anthropic Go
// SDK. It is never exercised by the unit tests (no API key in CI); the loop is
// tested against a scripted mock instead.
type anthropicClient struct {
	sdk anthropic.Client
}

// anthropicHTTPTimeout bounds individual HTTP calls to the Anthropic API.
// The builder loop enforces its own overall bound via ctx; this is a per-call
// backstop against a permanently stalled TCP connection.
const anthropicHTTPTimeout = 120 * time.Second

// NewAnthropicClient builds a Client over the Anthropic Messages API using the
// given key. An empty key falls back to the SDK's ANTHROPIC_API_KEY lookup.
func NewAnthropicClient(apiKey string) Client {
	httpClient := &http.Client{Timeout: anthropicHTTPTimeout}
	opts := []option.RequestOption{option.WithHTTPClient(httpClient)}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &anthropicClient{sdk: anthropic.NewClient(opts...)}
}

// Message translates the provider-agnostic request into SDK params, calls the
// Messages API, and flattens the response content blocks back into text +
// tool calls.
func (c *anthropicClient) Message(ctx context.Context, req MessageRequest) (MessageResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for _, td := range req.Tools {
		var schema anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(td.InputSchema, &schema); err != nil {
			return MessageResponse{}, err
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        td.Name,
			Description: anthropic.String(td.Description),
			InputSchema: schema,
		}})
	}

	params := anthropic.MessageNewParams{
		Model:     req.Model,
		MaxTokens: int64(maxTokens),
		Messages:  toSDKMessages(req.Messages),
		Tools:     tools,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	// tool_choice defaults to "auto" when tools are present and the field is
	// omitted, which is exactly the loop's intent; only non-auto choices would
	// need an explicit ToolChoice (reserved for later).

	msg, err := c.sdk.Messages.New(ctx, params)
	if err != nil {
		return MessageResponse{}, err
	}

	var out MessageResponse
	out.StopReason = string(msg.StopReason)
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			out.Text += block.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}
	return out, nil
}

// toSDKMessages maps the provider-agnostic conversation into SDK MessageParams,
// expanding each turn's text, tool calls, and tool results into content blocks.
func toSDKMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		var blocks []anthropic.ContentBlockParamUnion
		if m.Text != "" {
			blocks = append(blocks, anthropic.NewTextBlock(m.Text))
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, tc.Input, tc.Name))
		}
		for _, tr := range m.ToolResults {
			blocks = append(blocks, anthropic.NewToolResultBlock(tr.ToolCallID, tr.Content, tr.IsError))
		}
		if m.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		} else {
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out
}
