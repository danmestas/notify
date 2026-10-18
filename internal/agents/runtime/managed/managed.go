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

// The providers a managed agent can use. openai is any OpenAI-compatible
// chat endpoint, which includes models hosted locally (Ollama, vLLM,
// llama.cpp's server) as well as hosted services.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

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
	switch c.Provider {
	case ProviderOpenAI, ProviderAnthropic:
	default:
		return c, fmt.Errorf("managed agent provider %q is not one of %s, %s", c.Provider, ProviderOpenAI, ProviderAnthropic)
	}
	if strings.TrimSpace(c.Model) == "" {
		return c, errors.New("managed agent needs a model")
	}
	// No default endpoint: an agent's mail goes only where its owner points
	// it, never to a hosted service by omission.
	if c.Provider == ProviderOpenAI && strings.TrimSpace(c.BaseURL) == "" {
		return c, errors.New("managed agent with provider openai needs a baseUrl, e.g. http://localhost:11434/v1 for a local Ollama")
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
