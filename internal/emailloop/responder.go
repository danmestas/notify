package emailloop

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
)

// InboundTurn is one inbound email delivered to a responder: the resolved
// tenancy scope, the conversation thread it correlates to, the subscriber on
// the email side, and the message body (text part, quoted history stripped).
type InboundTurn struct {
	Scope        domain.Scope
	ThreadID     string
	SubscriberID string
	// AgentIdentifier is the responder agent for this thread (the default
	// responder dispatches the turn to it).
	AgentIdentifier string
	Text            string
}

// Responder is the pluggable seam that handles an inbound email turn. The
// default is AgentResponder, which threads the turn onto the conversation and
// lets the agent runtime generate a reply. Future responders ("forward to a
// webhook", "surface for a human") implement the same interface and are
// selected per env/integration without touching the inbound-parse path.
//
// Respond returns the conversation thread id the turn landed on (the inbox
// platform_thread_id) so the caller can record/log the correlation.
type Responder interface {
	Respond(ctx context.Context, turn InboundTurn) (threadID string, err error)
}

// ComposeFunc appends an inbound turn to the agent's conversation and starts
// the agent's turn run, returning the conversation's thread id. It is the
// in-process compose primitive (ingress.Service.ComposeMessage) reduced to the
// shape emailloop needs. A thin adapter at the wiring site bridges
// ingress.ComposeResult to the returned thread id, so emailloop does not import
// ingress (avoiding an import cycle, since ingress depends on emailloop via the
// reply hook).
type ComposeFunc func(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (threadID string, err error)

// AgentResponder is the default responder: it hands the inbound turn to the
// conversation's agent via the in-process compose primitive, so the same agent
// runtime that powers the inbox answers the email.
type AgentResponder struct {
	compose ComposeFunc
}

// NewAgentResponder builds the default agent responder over a compose function.
func NewAgentResponder(compose ComposeFunc) *AgentResponder {
	return &AgentResponder{compose: compose}
}

// Respond threads the inbound email onto the conversation and dispatches the
// agent turn. The conversation is keyed by ThreadID (the inbox
// platform_thread_id minted when the email-origin thread was first armed), so
// the reply continues the same conversation rather than opening a new one.
func (r *AgentResponder) Respond(ctx context.Context, turn InboundTurn) (string, error) {
	return r.compose(ctx, turn.Scope, turn.AgentIdentifier, turn.ThreadID, turn.SubscriberID, turn.Text)
}
