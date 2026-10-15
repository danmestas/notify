// Package wire is the JSON format of the agent API: the bodies the
// /v1/agents and /v1/inbox/conversations endpoints return. The domain types
// carry storage names and tenancy; these carry the camelCase names callers
// see, so either side can change without moving the other (#9). Tenancy
// (org and environment) is never part of the format.
package wire

import (
	"encoding/json"
	"log/slog"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
)

// Agent is an agent as the API returns it.
type Agent struct {
	ID             string          `json:"id"`
	Identifier     string          `json:"identifier"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Runtime        string          `json:"runtime"`
	Active         bool            `json:"active"`
	MeshSubject    string          `json:"meshSubject"`
	BridgeURL      string          `json:"bridgeUrl"`
	Managed        json.RawMessage `json:"managed,omitempty"`
	WelcomeMessage string          `json:"welcomeMessage"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
}

// FromAgent maps a stored agent.
func FromAgent(a agentdomain.Agent) Agent {
	return Agent{
		ID:             a.ID,
		Identifier:     a.Identifier,
		Name:           a.Name,
		Description:    a.Description,
		Runtime:        a.Runtime,
		Active:         a.Active,
		MeshSubject:    a.MeshSubject,
		BridgeURL:      a.BridgeURL,
		Managed:        a.Managed,
		WelcomeMessage: a.WelcomeMessage,
		CreatedAt:      a.CreatedAt,
		UpdatedAt:      a.UpdatedAt,
	}
}

// Link is an agent's link to a channel integration.
type Link struct {
	ID            string          `json:"id"`
	AgentID       string          `json:"agentId"`
	IntegrationID string          `json:"integrationId"`
	ConnectedAt   string          `json:"connectedAt"`
	Config        json.RawMessage `json:"config,omitempty"`
}

// FromLink maps a stored link.
func FromLink(l agentdomain.AgentLink) Link {
	return Link{
		ID:            l.ID,
		AgentID:       l.AgentID,
		IntegrationID: l.IntegrationID,
		ConnectedAt:   l.ConnectedAt,
		Config:        l.Config,
	}
}

// Conversation is one thread between an agent and a party. PlatformThreadID
// is the handle a caller passes back as conversationId when composing.
type Conversation struct {
	ID               string `json:"id"`
	AgentID          string `json:"agentId"`
	IntegrationID    string `json:"integrationId"`
	PlatformThreadID string `json:"platformThreadId"`
	SubscriberID     string `json:"subscriberId"`
	Status           string `json:"status"`
	CreatedAt        string `json:"createdAt"`
	LastActivityAt   string `json:"lastActivityAt"`
}

// FromConversation maps a stored conversation.
func FromConversation(c agentdomain.Conversation) Conversation {
	return Conversation{
		ID:               c.ID,
		AgentID:          c.AgentID,
		IntegrationID:    c.IntegrationID,
		PlatformThreadID: c.PlatformThreadID,
		SubscriberID:     c.SubscriberID,
		Status:           c.Status,
		CreatedAt:        c.CreatedAt,
		LastActivityAt:   c.LastActivityAt,
	}
}

// ConversationSummary is one row of the cross-agent conversation list.
type ConversationSummary struct {
	ConversationID   string `json:"conversationId"`
	AgentIdentifier  string `json:"agentIdentifier"`
	AgentName        string `json:"agentName"`
	SubscriberID     string `json:"subscriberId"`
	PlatformThreadID string `json:"platformThreadId"`
	Status           string `json:"status"`
	LastMessageAt    string `json:"lastMessageAt"`
	LastActivityID   string `json:"lastActivityId"`
}

// FromConversationSummary maps a conversation list row.
func FromConversationSummary(s agentstore.ConversationSummary) ConversationSummary {
	return ConversationSummary{
		ConversationID:   s.ConversationID,
		AgentIdentifier:  s.AgentIdentifier,
		AgentName:        s.AgentName,
		SubscriberID:     s.SubscriberID,
		PlatformThreadID: s.PlatformThreadID,
		Status:           s.Status,
		LastMessageAt:    s.LastMessageAt,
		LastActivityID:   s.LastActivityID,
	}
}

// Activity is one entry in a conversation's transcript.
type Activity struct {
	ID             string  `json:"id"`
	ConversationID string  `json:"conversationId"`
	Role           string  `json:"role"`
	Content        Content `json:"content"`
	RunID          string  `json:"runId,omitempty"`
	CreatedAt      string  `json:"createdAt"`
}

// FromActivity maps a stored activity, decoding its body into Content.
func FromActivity(a agentdomain.Activity) Activity {
	return Activity{
		ID:             a.ID,
		ConversationID: a.ConversationID,
		Role:           a.Role,
		Content:        decodeContent(a.ID, a.Content),
		RunID:          a.RunID,
		CreatedAt:      a.CreatedAt,
	}
}

// Content is a message body (#10). Parts are typed so kinds other than text
// can be added without breaking a client that reads only text parts.
type Content struct {
	Parts  []Part                       `json:"parts"`
	Action *agentdomain.SuggestedAction `json:"action,omitempty"`
}

// PartText is the type of a text part, the only kind written today.
const PartText = "text"

// Part is one fragment of a message body.
type Part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// decodeContent reads a stored body. One it cannot read becomes an empty
// body and a warning, so a single bad row never fails a transcript.
func decodeContent(activityID string, raw json.RawMessage) Content {
	var stored struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
		Action *agentdomain.SuggestedAction `json:"action"`
	}
	out := Content{Parts: []Part{}}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		slog.Warn("agent activity content is not a message body", "activity_id", activityID, "error", err)
		return out
	}
	for _, p := range stored.Parts {
		t := p.Type
		if t == "" {
			t = PartText
		}
		out.Parts = append(out.Parts, Part{Type: t, Text: p.Text})
	}
	out.Action = stored.Action
	return out
}

// Agents, Links, Conversations, ConversationSummaries and Activities map a
// list; the result is never nil, so an empty list encodes as [].

func Agents(in []agentdomain.Agent) []Agent { return mapAll(in, FromAgent) }

func Links(in []agentdomain.AgentLink) []Link { return mapAll(in, FromLink) }

func Conversations(in []agentdomain.Conversation) []Conversation {
	return mapAll(in, FromConversation)
}

func ConversationSummaries(in []agentstore.ConversationSummary) []ConversationSummary {
	return mapAll(in, FromConversationSummary)
}

func Activities(in []agentdomain.Activity) []Activity { return mapAll(in, FromActivity) }

func mapAll[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}
