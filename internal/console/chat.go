package console

// chat.go implements the read-only chat inbound status page (#106): a
// /console/chat surface that SHOWS the env-driven chat two-way loop config
// (Telegram long-poll, Discord gateway). It has no mutation routes — the loop
// is configured entirely from env (bot tokens + NOTIFY_CHAT_DEFAULT_*).
//
// All data comes from Deps.ChatStatus.Status (a chatloop.Snapshot). The console
// renders the snapshot; it never reaches into chatloop internals.

import (
	"net/http"

	"github.com/danmestas/notify/internal/domain"
)

// chatProviderRow is one provider's display row on the status page.
type chatProviderRow struct {
	// Name is the display name ("Telegram" / "Discord").
	Name string
	// Enabled is true when a bot token is configured.
	Enabled bool
	// Transport is the inbound transport description.
	Transport string
	// Guidance is the registration-model note (notify dials out — nothing to
	// register).
	Guidance string
}

// chatPageView powers the chat status page template.
type chatPageView struct {
	Header   pageHeader
	Rows     []chatProviderRow
	ReadOnly bool

	// DefaultAgent is the configured NOTIFY_CHAT_DEFAULT_AGENT (empty when
	// unset).
	DefaultAgent string
	// DefaultAgentResolved is true when the default agent exists in scope.
	DefaultAgentResolved bool
	// DefaultAgentActive is true when the resolved default agent is ACTIVE.
	DefaultAgentActive bool
}

// chatProviderDisplayName maps a chatloop provider key to its display name.
func chatProviderDisplayName(provider string) string {
	switch provider {
	case "telegram":
		return "Telegram"
	case "discord":
		return "Discord"
	default:
		return provider
	}
}

func (c *Console) serveChatPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveChatPage: w is nil")
	}
	if r == nil {
		panic("serveChatPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveChatPage: no scope in context")
	}

	snap := c.deps.ChatStatus.Status(r.Context(), scope)

	rows := make([]chatProviderRow, 0, len(snap.Providers))
	enabled := 0
	for _, p := range snap.Providers {
		if p.Enabled {
			enabled++
		}
		rows = append(rows, chatProviderRow{
			Name:      chatProviderDisplayName(p.Provider),
			Enabled:   p.Enabled,
			Transport: p.Transport,
			Guidance:  p.Guidance,
		})
	}

	header := pageHeader{
		Title:    "Chat Inbound",
		Subtitle: "Status of the two-way chat loop (Telegram long-poll, Discord gateway). Configured via environment variables.",
		Tiles: []tile{
			{Label: "enabled providers", Count: enabled, Tone: toneDefault},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Chat Inbound"}
	}

	view := chatPageView{
		Header:               header,
		Rows:                 rows,
		ReadOnly:             c.deps.ReadOnly,
		DefaultAgent:         snap.DefaultAgent,
		DefaultAgentResolved: snap.DefaultAgentResolved,
		DefaultAgentActive:   snap.DefaultAgentActive,
	}
	c.renderPage(w, r, http.StatusOK, "chat", "Chat Inbound", view)
}
