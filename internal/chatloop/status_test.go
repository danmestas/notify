package chatloop

import (
	"context"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
)

func TestStatus_TelegramEnabledDiscordDisabled_DefaultAgentActive(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Seed an ACTIVE agent matching NOTIFY_CHAT_DEFAULT_AGENT in the default
	// scope so Status resolves it.
	if _, err := agentstore.New(core).Agents().Create(context.Background(), scope, agentdomain.Agent{
		Identifier: "support",
		Name:       "Support",
		Runtime:    "mesh",
		Active:     true,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Telegram token set, Discord empty, default agent "support".
	svc := New(core, &fakeResponder{}, "tg-token", "", "support", nil)

	snap := svc.Status(context.Background(), scope)

	if len(snap.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(snap.Providers))
	}

	var tg, dc ProviderStatus
	for _, p := range snap.Providers {
		switch p.Provider {
		case providerTelegram:
			tg = p
		case providerDiscord:
			dc = p
		}
	}

	if !tg.Enabled {
		t.Errorf("telegram should be enabled (token set)")
	}
	if tg.Transport != "long-poll (getUpdates)" {
		t.Errorf("telegram transport = %q, want %q", tg.Transport, "long-poll (getUpdates)")
	}
	if dc.Enabled {
		t.Errorf("discord should be disabled (no token)")
	}
	if dc.Transport != "gateway WebSocket" {
		t.Errorf("discord transport = %q, want %q", dc.Transport, "gateway WebSocket")
	}
	// Both providers dial out — no webhook to register.
	for _, p := range snap.Providers {
		if p.Guidance == "" {
			t.Errorf("provider %q missing inbound guidance", p.Provider)
		}
	}

	if snap.DefaultAgent != "support" {
		t.Errorf("DefaultAgent = %q, want support", snap.DefaultAgent)
	}
	if !snap.DefaultAgentResolved {
		t.Errorf("DefaultAgentResolved = false, want true (active agent seeded)")
	}
	if !snap.DefaultAgentActive {
		t.Errorf("DefaultAgentActive = false, want true")
	}
}

func TestStatus_NoDefaultAgent_DropsColdInbound(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// No default agent configured at all.
	svc := New(core, &fakeResponder{}, "tg-token", "dc-token", "", nil)

	snap := svc.Status(context.Background(), scope)

	if snap.DefaultAgent != "" {
		t.Errorf("DefaultAgent = %q, want empty", snap.DefaultAgent)
	}
	if snap.DefaultAgentResolved {
		t.Errorf("DefaultAgentResolved = true, want false (none configured)")
	}
	if snap.DefaultAgentActive {
		t.Errorf("DefaultAgentActive = true, want false (cold inbound dropped)")
	}
}

func TestStatus_DefaultAgentSetButUnresolved(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Default agent name set but no matching agent row exists.
	svc := New(core, &fakeResponder{}, "", "dc-token", "ghost", nil)

	snap := svc.Status(context.Background(), scope)

	if snap.DefaultAgent != "ghost" {
		t.Errorf("DefaultAgent = %q, want ghost", snap.DefaultAgent)
	}
	if snap.DefaultAgentResolved {
		t.Errorf("DefaultAgentResolved = true, want false (no matching agent)")
	}
	if snap.DefaultAgentActive {
		t.Errorf("DefaultAgentActive = true, want false")
	}
}

func TestStatus_DefaultAgentInactive(t *testing.T) {
	core := openTempStore(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := agentstore.New(core).Agents().Create(context.Background(), scope, agentdomain.Agent{
		Identifier: "support",
		Name:       "Support",
		Runtime:    "mesh",
		Active:     false,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	svc := New(core, &fakeResponder{}, "tg-token", "", "support", nil)

	snap := svc.Status(context.Background(), scope)

	if !snap.DefaultAgentResolved {
		t.Errorf("DefaultAgentResolved = false, want true (agent exists)")
	}
	if snap.DefaultAgentActive {
		t.Errorf("DefaultAgentActive = true, want false (agent inactive)")
	}
}
