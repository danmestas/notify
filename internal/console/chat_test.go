package console

import (
	"net/http"
	"strings"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
)

// ─── chat inbound status page (#106) ─────────────────────────────────────────

func TestChat_StatusPageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/chat", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/chat: status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Chat Inbound") {
		t.Errorf("page missing 'Chat Inbound' heading")
	}
	// Both providers must appear.
	if !strings.Contains(body, "Telegram") {
		t.Errorf("page missing Telegram provider")
	}
	if !strings.Contains(body, "Discord") {
		t.Errorf("page missing Discord provider")
	}
}

func TestChat_ShowsEnabledAndDisabledProviders(t *testing.T) {
	tc := newTestConsole(t)
	// Telegram token set, Discord empty (default stub config below).
	tc.setChatStatus(telegramToken, "", "")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/chat", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	// Enabled chip for Telegram, disabled for Discord.
	if !strings.Contains(body, "Enabled") {
		t.Errorf("expected an 'Enabled' chip for the configured provider")
	}
	if !strings.Contains(body, "Disabled") {
		t.Errorf("expected a 'Disabled' chip for the unconfigured provider")
	}
}

func TestChat_ShowsTransportAndNoWebhookGuidance(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/chat", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "long-poll (getUpdates)") {
		t.Errorf("missing Telegram transport text")
	}
	if !strings.Contains(body, "gateway WebSocket") {
		t.Errorf("missing Discord transport text")
	}
	// The page must correct the misleading webhook guidance.
	if !strings.Contains(body, "no webhook") && !strings.Contains(body, "dials out") {
		t.Errorf("missing 'notify dials out — no webhook to register' guidance")
	}
}

func TestChat_NoDefaultAgent_ShowsColdInboundDroppedWarning(t *testing.T) {
	tc := newTestConsole(t)
	// No default agent configured.
	tc.setChatStatus(telegramToken, "", "")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/chat", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "No default agent") && !strings.Contains(body, "cold inbound is dropped") {
		t.Errorf("missing 'no default agent → cold inbound dropped' warning; body=%s", body)
	}
}

func TestChat_DefaultAgentActive_ShownAsResolved(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedAgent(t, agentdomain.Agent{
		Identifier: "support",
		Name:       "Support",
		Runtime:    "mesh",
		Active:     true,
	})
	tc.setChatStatus(telegramToken, "", "support")
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/chat", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "support") {
		t.Errorf("page missing default agent identifier")
	}
	if !strings.Contains(body, "Active") {
		t.Errorf("page should show the resolved agent as Active")
	}
}

func TestChat_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/chat", cookie)
	if !strings.Contains(rr.Body.String(), `href="/console/chat"`) {
		t.Errorf("chat nav link missing")
	}
}

func TestChat_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/chat", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("GET /console/chat unauthenticated: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("GET /console/chat: Location = %q, want /console/login", got)
	}
}
