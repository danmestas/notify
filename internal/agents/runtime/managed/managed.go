// Package managed runs agents notify hosts itself: a model, a system prompt
// and the conversation so far, with no code of the agent owner's. Each agent
// brings its own model settings and API key, kept on the agent with the key
// sealed at rest. A turn is one model call; the turn already runs as a durable
// engine step, so a failed call is retried and a restart resumes it.
package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/secretbox"
)

// A managed agent can use any provider. anthropic speaks Anthropic's own
// API; every other provider is reached over the OpenAI-compatible chat API,
// which almost every hosted service and every local model server speaks.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// providerBaseURLs are the OpenAI-compatible endpoints of providers people
// commonly name, so naming one is enough. Any other provider needs a baseUrl,
// and a baseUrl always wins over the preset. A preset is used only when the
// owner names its provider: nothing is sent to a hosted service by omission.
var providerBaseURLs = map[string]string{
	ProviderOpenAI: "https://api.openai.com/v1",
	"openrouter":   "https://openrouter.ai/api/v1",
	"groq":         "https://api.groq.com/openai/v1",
	"together":     "https://api.together.xyz/v1",
	"mistral":      "https://api.mistral.ai/v1",
	"deepseek":     "https://api.deepseek.com/v1",
	"xai":          "https://api.x.ai/v1",
	"gemini":       "https://generativelanguage.googleapis.com/v1beta/openai",
	"fireworks":    "https://api.fireworks.ai/inference/v1",
	"perplexity":   "https://api.perplexity.ai",
	"ollama":       "http://localhost:11434/v1",
	"lmstudio":     "http://localhost:1234/v1",
}

// Output limits for one reply.
const (
	defaultMaxTokens = 1024
	maxMaxTokens     = 8192
)

// Config is a managed agent's settings, stored as the agent's managed JSON.
// APIKey is sealed by the API before it is stored.
type Config struct {
	Provider     string `json:"provider"`
	BaseURL      string `json:"baseUrl,omitempty"`
	Model        string `json:"model"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
	MaxTokens    int    `json:"maxTokens,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
}

// ParseConfig reads and checks a managed agent's settings.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, errors.New("managed agent has no settings: set provider and model")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("managed agent settings are not valid JSON: %w", err)
	}
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	if c.Provider == "" {
		return c, errors.New("managed agent needs a provider, e.g. openrouter, openai, anthropic or ollama")
	}
	if strings.TrimSpace(c.Model) == "" {
		return c, errors.New("managed agent needs a model")
	}
	if c.Provider != ProviderAnthropic && c.BaseURL == "" {
		preset, ok := providerBaseURLs[c.Provider]
		if !ok {
			return c, fmt.Errorf("managed agent provider %q needs a baseUrl: the address of its OpenAI-compatible API", c.Provider)
		}
		c.BaseURL = preset
	}
	if c.MaxTokens < 0 || c.MaxTokens > maxMaxTokens {
		return c, fmt.Errorf("managed agent maxTokens must be 0-%d", maxMaxTokens)
	}
	return c, nil
}

// NewClient builds the model client for a config; replaceable in tests.
type NewClient func(c Config, apiKey string) aibuilder.Client

// DefaultClient builds the real model client.
func DefaultClient(c Config, apiKey string) aibuilder.Client {
	if c.Provider == ProviderAnthropic {
		return aibuilder.NewAnthropicClient(apiKey)
	}
	return aibuilder.NewOpenAIClient(apiKey, c.BaseURL, c.Model)
}

// Executor runs managed agent turns.
type Executor struct {
	box       *secretbox.Box
	newClient NewClient
}

// New returns an executor that opens sealed keys with box and calls models
// through newClient.
func New(box *secretbox.Box, newClient NewClient) *Executor {
	return &Executor{box: box, newClient: newClient}
}

// Dispatch answers one turn with one model call.
func (e *Executor) Dispatch(ctx context.Context, t agentdomain.Turn) (agentdomain.Reply, error) {
	cfg, err := ParseConfig(t.Agent.Managed)
	if err != nil {
		return agentdomain.Reply{}, agentdomain.NewPermanentError(err)
	}
	apiKey := cfg.APIKey
	if secretbox.IsSealed(apiKey) {
		if e.box == nil {
			return agentdomain.Reply{}, agentdomain.NewPermanentError(errors.New("managed agent key is sealed but this server has no master key"))
		}
		if apiKey, err = e.box.Open(apiKey); err != nil {
			return agentdomain.Reply{}, agentdomain.NewPermanentError(fmt.Errorf("open managed agent key: %w", err))
		}
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	msgs := conversation(t.History, t.Message)
	if len(msgs) == 0 {
		return agentdomain.Reply{}, agentdomain.NewPermanentError(errors.New("managed agent turn has no message to answer"))
	}
	resp, err := e.newClient(cfg, apiKey).Message(ctx, aibuilder.MessageRequest{
		Model:     cfg.Model,
		System:    cfg.SystemPrompt,
		Messages:  msgs,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return agentdomain.Reply{}, fmt.Errorf("managed agent model call: %w", err) // transient: the engine retries
	}
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return agentdomain.Reply{}, errors.New("managed agent model returned no text")
	}
	return agentdomain.Reply{Parts: []agentdomain.Part{{Text: text}}}, nil
}

// conversation turns the transcript and the new message into model messages:
// the agent's own activities are the assistant side, everything else the user
// side. Consecutive messages from one side are joined, because some providers
// require the two sides to alternate.
func conversation(history []agentdomain.Activity, msg agentdomain.Activity) []aibuilder.Message {
	var out []aibuilder.Message
	add := func(a agentdomain.Activity) {
		text := activityText(a)
		if text == "" {
			return
		}
		role := aibuilder.RoleUser
		if a.Role == "agent" {
			role = aibuilder.RoleAssistant
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Text += "\n\n" + text
			return
		}
		out = append(out, aibuilder.Message{Role: role, Text: text})
	}
	for _, a := range history {
		add(a)
	}
	add(msg)
	// A conversation must open with the user side.
	for len(out) > 0 && out[0].Role != aibuilder.RoleUser {
		out = out[1:]
	}
	return out
}

func activityText(a agentdomain.Activity) string {
	var c struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(a.Content, &c); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}
