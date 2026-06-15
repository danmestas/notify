package chatloop

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
)

// InboundTurn is one inbound chat message delivered to a responder: the
// resolved tenancy scope, the conversation thread it correlates to (empty for a
// cold message that mints a new conversation), the subscriber on the chat side,
// the responder agent, and the message text.
type InboundTurn struct {
	Scope domain.Scope
	// ConversationID is the conversation thread to continue, or empty to mint a
	// new one (cold inbound on a chat with no existing binding).
	ConversationID string
	SubscriberID   string
	// AgentIdentifier is the responder agent for this turn (the default
	// responder dispatches the turn to it).
	AgentIdentifier string
	Text            string
}

// Responder is the pluggable seam that handles an inbound chat turn. The
// default is AgentResponder, which threads the turn onto the conversation and
// lets the agent runtime generate a reply. Mirrors emailloop.Responder.
//
// Respond returns the conversation thread id the turn landed on so the caller
// can bind the chat_thread to it.
type Responder interface {
	Respond(ctx context.Context, turn InboundTurn) (conversationID string, err error)
}

// ComposeFunc appends an inbound turn to the agent's conversation and starts
// the agent's turn run, returning the conversation's thread id. It is the
// in-process compose primitive (ingress.Service.ComposeMessage) reduced to the
// shape chatloop needs. A thin adapter at the wiring site bridges
// ingress.ComposeResult to the returned thread id, so chatloop does not import
// ingress (avoiding an import cycle, since ingress depends on chatloop via the
// reply hook).
type ComposeFunc func(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (conversationID2 string, err error)

// AgentResponder is the default responder: it hands the inbound turn to the
// conversation's agent via the in-process compose primitive, so the same agent
// runtime that powers the inbox answers the chat.
type AgentResponder struct {
	compose ComposeFunc
}

// NewAgentResponder builds the default agent responder over a compose function.
func NewAgentResponder(compose ComposeFunc) *AgentResponder {
	return &AgentResponder{compose: compose}
}

// Respond threads the inbound chat onto the conversation and dispatches the
// agent turn. An empty ConversationID mints a new conversation; a non-empty one
// continues the existing thread.
func (r *AgentResponder) Respond(ctx context.Context, turn InboundTurn) (string, error) {
	return r.compose(ctx, turn.Scope, turn.AgentIdentifier, turn.ConversationID, turn.SubscriberID, turn.Text)
}
