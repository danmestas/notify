package chatloop

// status.go exposes a read-only snapshot of the chat two-way loop's
// configuration for the operator console's /console/chat status page (#106).
//
// The loop is configured entirely from env (bot tokens + NOTIFY_CHAT_DEFAULT_*),
// so the snapshot is derived from the Service's existing config fields plus a
// single agents-store lookup to resolve the default responder. There is no
// store, migration, or mutation — the console only SHOWS what is configured.

import (
	"context"

	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
)

// Per-provider transport and guidance copy. For both providers notify dials
// OUT (Telegram long-poll getUpdates; Discord gateway WebSocket), so there is
// no inbound webhook or public tunnel to register — the console states this
// explicitly to correct the misleading "/v1/inbound/<id>" guidance.
const (
	telegramTransport = "long-poll (getUpdates)"
	discordTransport  = "gateway WebSocket"

	// chatInboundGuidance is shared by both providers: notify connects
	// outbound, so the operator registers nothing.
	chatInboundGuidance = "notify dials out — no webhook or tunnel to register"
)

// ProviderStatus is the read-only status of one chat provider (Telegram or
// Discord) as configured on the loop.
type ProviderStatus struct {
	// Provider is the lowercase provider key ("telegram" / "discord").
	Provider string
	// Enabled is true when a bot token is configured for this provider.
	Enabled bool
	// Transport is the human-readable inbound transport ("long-poll
	// (getUpdates)" / "gateway WebSocket").
	Transport string
	// Guidance explains the registration model (notify dials out — nothing to
	// register). Same string for both providers today.
	Guidance string
}

// Snapshot is the whole-loop status the console renders. The provider list is
// always both providers (Telegram, Discord) so the page shows each one's
// enabled/disabled state. The default-responder fields describe the
// cold-inbound responder NOTIFY_CHAT_DEFAULT_AGENT resolves to in the default
// scope.
type Snapshot struct {
	// Providers is the per-provider status, always [Telegram, Discord].
	Providers []ProviderStatus
	// DefaultAgent is the configured NOTIFY_CHAT_DEFAULT_AGENT identifier
	// (empty when unset).
	DefaultAgent string
	// DefaultAgentResolved is true when DefaultAgent is set AND resolves to an
	// existing agent row in the default scope.
	DefaultAgentResolved bool
	// DefaultAgentActive is true when the resolved default agent is ACTIVE.
	// When DefaultAgent is unset or unresolved this is false, meaning cold
	// inbound on an unbound chat is dropped.
	DefaultAgentActive bool
}

// Status returns the loop's read-only configuration snapshot. scope is the
// default chat scope (NOTIFY_CHAT_DEFAULT_ORG / _ENV) the default agent is
// looked up in. The single agents-store lookup degrades gracefully: a missing
// or errored agent leaves DefaultAgentResolved/Active false (cold inbound
// dropped) rather than failing the page.
func (s *Service) Status(ctx context.Context, scope domain.Scope) Snapshot {
	snap := Snapshot{
		Providers: []ProviderStatus{
			{
				Provider:  providerTelegram,
				Enabled:   s.telegramToken != "",
				Transport: telegramTransport,
				Guidance:  chatInboundGuidance,
			},
			{
				Provider:  providerDiscord,
				Enabled:   s.discordToken != "",
				Transport: discordTransport,
				Guidance:  chatInboundGuidance,
			},
		},
		DefaultAgent: s.defaultAgent,
	}

	if s.defaultAgent == "" || s.core == nil {
		return snap
	}

	agent, err := agentstore.New(s.core).Agents().GetByIdentifier(ctx, scope, s.defaultAgent)
	if err != nil {
		// Unresolved default agent: cold inbound is dropped. Leave the resolved
		// flags false so the console renders the "no default agent" warning.
		return snap
	}
	snap.DefaultAgentResolved = true
	snap.DefaultAgentActive = agent.Active
	return snap
}
