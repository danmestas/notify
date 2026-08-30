package chatloop

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
)

// InboundTurn is one inbound chat message delivered to a responder: the
// resolved tenancy scope, the conversation thread it correlates to (always set:
// handleInbound mints and binds it before dispatch), the subscriber on the chat
// side, the responder agent, and the message text.
type InboundTurn struct {
	Scope domain.Scope
	// ConversationID is the conversation thread to continue. For a cold chat
	// chatloop mints it and commits the chat_thread binding before dispatch,
	// so the compose layer upserts (rather than mints) the conversation.
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
// Respond returns the conversation thread id the turn landed on; the caller
// has already bound the chat_thread to turn.ConversationID and only uses the
// return to flag a responder that landed elsewhere.
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
// agent turn (the compose layer upserts the conversation by its thread id).
func (r *AgentResponder) Respond(ctx context.Context, turn InboundTurn) (string, error) {
	return r.compose(ctx, turn.Scope, turn.AgentIdentifier, turn.ConversationID, turn.SubscriberID, turn.Text)
}
