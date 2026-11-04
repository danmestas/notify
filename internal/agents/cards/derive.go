package cards

import (
	"encoding/json"

	"github.com/danmestas/notify/internal/agents/domain"
)

// RuntimeExtensionURI identifies the dagnats-agents runtime extension a card
// carries its model and tools under.
const RuntimeExtensionURI = "https://github.com/danmestas/dagnats-agents/blob/main/docs/a2a/ext/runtime/v1.md"

type tool struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type extension struct {
	URI      string         `json:"uri"`
	Required bool           `json:"required"`
	Params   map[string]any `json:"params"`
}

type capabilities struct {
	Streaming  bool        `json:"streaming"`
	Extensions []extension `json:"extensions"`
}

type agentCard struct {
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	Version            string       `json:"version"`
	ProtocolVersion    string       `json:"protocolVersion"`
	URL                string       `json:"url"`
	PreferredTransport string       `json:"preferredTransport"`
	Capabilities       capabilities `json:"capabilities"`
	DefaultInputModes  []string     `json:"defaultInputModes"`
	DefaultOutputModes []string     `json:"defaultOutputModes"`
	Skills             []struct{}   `json:"skills"`
}

// Derive builds a strict A2A v0.3 card for a managed agent from its record.
// It is for the response path only and never stored. It names the model, the
// LLM provider and the MCP servers as tools; the API key, base URL, system
// prompt and server credentials are never read into it. It returns nil for
// an agent that is not managed or whose settings are unreadable.
func Derive(a domain.Agent) json.RawMessage {
	if a.Runtime != domain.RuntimeManaged {
		return nil
	}
	var m struct {
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		MCPServers []struct {
			Name string `json:"name"`
		} `json:"mcpServers"`
	}
	if len(a.Managed) > 0 {
		if err := json.Unmarshal(a.Managed, &m); err != nil {
			return nil
		}
	}
	tools := make([]tool, 0, len(m.MCPServers))
	for _, s := range m.MCPServers {
		tools = append(tools, tool{Name: s.Name, Kind: "mcp"})
	}
	card := agentCard{
		Name:               a.Name,
		Description:        a.Description,
		Version:            a.UpdatedAt,
		ProtocolVersion:    "0.3.0",
		PreferredTransport: "JSONRPC",
		Capabilities: capabilities{Extensions: []extension{{
			URI: RuntimeExtensionURI,
			Params: map[string]any{
				"model":       m.Model,
				"llmProvider": m.Provider,
				"tools":       tools,
			},
		}}},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []struct{}{},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return nil
	}
	return raw
}
