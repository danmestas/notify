package aibuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// DefaultOpenAIBaseURL is the base URL used when NOTIFY_AI_BASE_URL is unset.
// It points at OpenRouter, which speaks the OpenAI Chat Completions protocol
// and proxies to many provider models behind one key.
const DefaultOpenAIBaseURL = "https://openrouter.ai/api/v1"

// DefaultOpenAIModel is the model slug used when NOTIFY_AI_MODEL is unset.
// It is an OpenRouter slug; override with NOTIFY_AI_MODEL to target any other
// model the endpoint supports (e.g. "openai/gpt-4o" on OpenAI directly).
const DefaultOpenAIModel = "openai/gpt-4.1"

// openaiHTTPTimeout mirrors anthropicHTTPTimeout: a per-call backstop that
// prevents a permanently stalled TCP connection from blocking the builder.
const openaiHTTPTimeout = 120 * time.Second

// openaiClient is a Client backed by any OpenAI-compatible Chat Completions
// endpoint (OpenRouter, OpenAI, Azure via OpenAI compat, …). It is selected
// when NOTIFY_AI_API_KEY is set in the environment.
type openaiClient struct {
	sdk     openai.Client
	model   string
	baseURL string // stored so tests can inspect it without making a real request
}

// NewOpenAIClient builds a Client over an OpenAI-compatible Chat Completions
// endpoint. Empty baseURL defaults to DefaultOpenAIBaseURL (OpenRouter);
// empty model defaults to DefaultOpenAIModel.
func NewOpenAIClient(apiKey, baseURL, model string) Client {
	if baseURL == "" {
		baseURL = DefaultOpenAIBaseURL
	}
	if model == "" {
		model = DefaultOpenAIModel
	}
	httpClient := &http.Client{Timeout: openaiHTTPTimeout}
	opts := []option.RequestOption{
		option.WithHTTPClient(httpClient),
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		// OpenRouter attribution headers — harmless on other endpoints.
		option.WithHeader("HTTP-Referer", "https://github.com/danmestas/notify"),
		option.WithHeader("X-Title", "Notify AI Builder"),
	}
	return &openaiClient{
		sdk:     openai.NewClient(opts...),
		model:   model,
		baseURL: baseURL,
	}
}

// Message translates the provider-agnostic request into an OpenAI Chat
// Completions call, then flattens the response back into the neutral types.
func (c *openaiClient) Message(ctx context.Context, req MessageRequest) (MessageResponse, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}

	// Apply the same default-token backstop as anthropicClient.
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	completionParams := openai.ChatCompletionNewParams{
		Model:     model,
		Messages:  toOpenAIMessages(req.System, req.Messages),
		MaxTokens: openai.Int(int64(maxTokens)),
	}

	if len(req.Tools) > 0 {
		tools := make([]openai.ChatCompletionToolParam, len(req.Tools))
		for i, td := range req.Tools {
			var schema openai.FunctionParameters
			if err := json.Unmarshal(td.InputSchema, &schema); err != nil {
				return MessageResponse{}, err
			}
			tools[i] = openai.ChatCompletionToolParam{
				Function: openai.FunctionDefinitionParam{
					Name:        td.Name,
					Description: openai.String(td.Description),
					Parameters:  schema,
				},
			}
		}
		completionParams.Tools = tools
	}

	// "auto" is the only ToolChoice the loop uses today; map it to the SDK's
	// union type. An empty ToolChoice with tools present also defaults to auto.
	if req.ToolChoice == "auto" || (req.ToolChoice == "" && len(req.Tools) > 0) {
		completionParams.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String("auto"),
		}
	}

	resp, err := c.sdk.Chat.Completions.New(ctx, completionParams)
	if err != nil {
		return MessageResponse{}, err
	}

	if len(resp.Choices) == 0 {
		return MessageResponse{}, nil
	}
	choice := resp.Choices[0]
	out := MessageResponse{
		StopReason: choice.FinishReason,
		Text:       choice.Message.Content,
	}
	for _, tc := range choice.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out, nil
}

// toOpenAIMessages maps the provider-agnostic conversation into the OpenAI
// message slice. Key invariants:
//   - System prompt is always the first message when present.
//   - An assistant turn with ToolCalls is one message (content + tool_calls).
//   - A user turn with ToolResults expands into one tool-role message per
//     result, each carrying the matching tool_call_id for the round-trip.
func toOpenAIMessages(system string, msgs []Message) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	if system != "" {
		out = append(out, openai.SystemMessage(system))
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleUser:
			if m.Text != "" {
				out = append(out, openai.UserMessage(m.Text))
			}
			// Each ToolResult becomes its own tool-role message; OpenAI requires
			// one message per tool_call_id.
			for _, tr := range m.ToolResults {
				out = append(out, openai.ToolMessage(tr.Content, tr.ToolCallID))
			}
		case RoleAssistant:
			// Build the assistant param directly so we can carry both optional
			// text content and optional tool_calls in a single message.
			var asst openai.ChatCompletionAssistantMessageParam
			if m.Text != "" {
				asst.Content.OfString = openai.String(m.Text)
			}
			if len(m.ToolCalls) > 0 {
				asst.ToolCalls = make([]openai.ChatCompletionMessageToolCallParam, len(m.ToolCalls))
				for i, tc := range m.ToolCalls {
					asst.ToolCalls[i] = openai.ChatCompletionMessageToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: string(tc.Input),
						},
					}
				}
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		}
	}
	return out
}

// NewClientFromEnv selects and returns a Client based on environment variables.
//
//   - NOTIFY_AI_API_KEY set → openaiClient (OpenAI-compatible, defaults to
//     OpenRouter). NOTIFY_AI_BASE_URL and NOTIFY_AI_MODEL override the endpoint
//     and model respectively.
//   - NOTIFY_AI_API_KEY unset, ANTHROPIC_API_KEY set → anthropicClient.
//   - Neither set → nil (builder gracefully disabled).
func NewClientFromEnv(getenv func(string) string) Client {
	if key := getenv("NOTIFY_AI_API_KEY"); key != "" {
		baseURL := getenv("NOTIFY_AI_BASE_URL")
		model := getenv("NOTIFY_AI_MODEL")
		return NewOpenAIClient(key, baseURL, model)
	}
	if key := getenv("ANTHROPIC_API_KEY"); key != "" {
		return NewAnthropicClient(key)
	}
	return nil
}
