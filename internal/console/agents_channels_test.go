package console

// agents_channels_test.go covers the agent↔channel linking UI on the agent
// detail "Channels" tab (#104): the link/unlink mutation handlers and the
// channels-tab render (linked-integration list, link picker, setup/webhook
// URL). The handlers are thin wrappers over the same agents-store Links
// service the /v1 API uses, so the assertions check both the HTTP contract
// (303 redirects, scope guards) and the persisted store state.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

// seedChatIntegration seeds one chat integration under the test scope.
func seedChatIntegration(t *testing.T, tc *testConsole, identifier, provider string) domain.Integration {
	t.Helper()
	return tc.seedIntegration(t, domain.Integration{
		ProviderID:     provider,
		Channel:        "chat",
		Identifier:     identifier,
		EncCredentials: []byte("fake-ciphertext"),
		Active:         true,
	})
}

// ─── link ──────────────────────────────────────────────────────────────────

func TestAgents_ChannelLink_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	agent := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")

	form := url.Values{"csrf_token": {csrf}, "integrationId": {in.ID}}
	rr := tc.postForm(t, "/console/agents/support-bot/channels", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/console/agents/support-bot/tabs/channels" {
		t.Errorf("Location = %q, want channels tab", loc)
	}

	links, err := tc.agentStore.Links().ListByAgent(testBackground(), tc.scope, agent.ID)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 1 || links[0].IntegrationID != in.ID {
		t.Fatalf("expected link to %s, got %+v", in.ID, links)
	}
}

func TestAgents_ChannelLink_AppearsInTab(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")

	form := url.Values{"csrf_token": {csrf}, "integrationId": {in.ID}}
	if rr := tc.postForm(t, "/console/agents/support-bot/channels", form, cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("link status = %d, want 303", rr.Code)
	}

	rr := tc.get(t, "/console/agents/support-bot/tabs/channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, in.ID) {
		t.Errorf("linked integration id not listed; body:\n%s", body)
	}
	if !strings.Contains(body, "telegram-prod") {
		t.Errorf("linked integration identifier not shown; body:\n%s", body)
	}
}

func TestAgents_ChannelLink_NonexistentIntegration(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	form := url.Values{"csrf_token": {csrf}, "integrationId": {"int-does-not-exist"}}
	rr := tc.postForm(t, "/console/agents/support-bot/channels", form, cookie)
	// A missing integration must not panic; it redirects back with an error.
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (clean error); body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want ?error= surfaced", loc)
	}
}

func TestAgents_ChannelLink_NonexistentAgent(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}, "integrationId": {"int-x"}}
	rr := tc.postForm(t, "/console/agents/ghost/channels", form, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown agent", rr.Code)
	}
}

func TestAgents_ChannelLink_TenantIsolation(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	// An integration belonging to another tenant must not be linkable.
	other := tc.altScope(t)
	otherIn, err := tc.st.Integrations().Create(testBackground(), other, domain.Integration{
		ProviderID:     "telegram",
		Channel:        "chat",
		Identifier:     "foreign-tg",
		EncCredentials: []byte("fake"),
		Active:         true,
	})
	if err != nil {
		t.Fatalf("seed foreign integration: %v", err)
	}

	form := url.Values{"csrf_token": {csrf}, "integrationId": {otherIn.ID}}
	rr := tc.postForm(t, "/console/agents/support-bot/channels", form, cookie)
	// The foreign integration is not visible in this scope → clean error, no link.
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (clean error)", rr.Code)
	}
	agent, _ := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "support-bot")
	links, _ := tc.agentStore.Links().ListByAgent(testBackground(), tc.scope, agent.ID)
	if len(links) != 0 {
		t.Errorf("foreign integration was linked across tenants: %+v", links)
	}
}

func TestAgents_ChannelLink_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}, "integrationId": {in.ID}}
	rr := tc.postForm(t, "/console/agents/support-bot/channels", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

// ─── unlink ────────────────────────────────────────────────────────────────

func TestAgents_ChannelUnlink_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	agent := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")

	if _, err := tc.agentStore.Links().Link(testBackground(), tc.scope, agentdomain.AgentLink{
		AgentID:       agent.ID,
		IntegrationID: in.ID,
		Config:        json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	form := url.Values{"csrf_token": {csrf}}
	rr := tc.postForm(t, "/console/agents/support-bot/channels/"+in.ID+"/unlink", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/console/agents/support-bot/tabs/channels" {
		t.Errorf("Location = %q, want channels tab", loc)
	}

	links, _ := tc.agentStore.Links().ListByAgent(testBackground(), tc.scope, agent.ID)
	if len(links) != 0 {
		t.Errorf("integration still linked after unlink: %+v", links)
	}
}

func TestAgents_ChannelUnlink_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	agent := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")
	if _, err := tc.agentStore.Links().Link(testBackground(), tc.scope, agentdomain.AgentLink{
		AgentID: agent.ID, IntegrationID: in.ID, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}}
	rr := tc.postForm(t, "/console/agents/support-bot/channels/"+in.ID+"/unlink", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

// ─── setup / webhook URL ─────────────────────────────────────────────────────

// TestAgents_ChannelsTab_RendersSetupURL_PlaceholderWhenNoBaseURL pins the
// graceful-degradation path: with no PublicBaseURL configured the tab renders
// the literal "{base_url}" placeholder pattern the operator fills in, mirroring
// the /v1 API's channelSetup contract.
func TestAgents_ChannelsTab_RendersSetupURL_PlaceholderWhenNoBaseURL(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.PublicBaseURL = "" // explicit: no public origin configured
	cookie, _ := tc.login(t)
	agent := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")
	if _, err := tc.agentStore.Links().Link(testBackground(), tc.scope, agentdomain.AgentLink{
		AgentID: agent.ID, IntegrationID: in.ID, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	rr := tc.get(t, "/console/agents/support-bot/tabs/channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	wantURL := "{base_url}/v1/inbound/" + in.ID
	if !strings.Contains(body, wantURL) {
		t.Errorf("placeholder setup URL %q not rendered; body:\n%s", wantURL, body)
	}
}

// TestAgents_ChannelsTab_RendersSetupURL_SubstitutesConfiguredBaseURL guards
// the main.go wiring (#104): when PublicBaseURL is set on Deps the channels tab
// must render the substituted origin, not the "{base_url}" placeholder. This is
// the regression net for the dead-wire the review flagged.
func TestAgents_ChannelsTab_RendersSetupURL_SubstitutesConfiguredBaseURL(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.deps.PublicBaseURL = "https://notify.example.com"
	cookie, _ := tc.login(t)
	agent := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	in := seedChatIntegration(t, tc, "telegram-prod", "telegram")
	if _, err := tc.agentStore.Links().Link(testBackground(), tc.scope, agentdomain.AgentLink{
		AgentID: agent.ID, IntegrationID: in.ID, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	rr := tc.get(t, "/console/agents/support-bot/tabs/channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	wantURL := "https://notify.example.com/v1/inbound/" + in.ID
	if !strings.Contains(body, wantURL) {
		t.Errorf("substituted setup URL %q not rendered; body:\n%s", wantURL, body)
	}
	if strings.Contains(body, "{base_url}") {
		t.Errorf("placeholder {base_url} still rendered despite configured PublicBaseURL; body:\n%s", body)
	}
}

// ─── link picker ─────────────────────────────────────────────────────────────

func TestAgents_ChannelsTab_LinkPickerOffersUnlinked(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	agent := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	linked := seedChatIntegration(t, tc, "telegram-linked", "telegram")
	unlinked := seedChatIntegration(t, tc, "discord-free", "discord")
	if _, err := tc.agentStore.Links().Link(testBackground(), tc.scope, agentdomain.AgentLink{
		AgentID: agent.ID, IntegrationID: linked.ID, Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	rr := tc.get(t, "/console/agents/support-bot/tabs/channels?link=true", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// The link form posts to the link route.
	if !strings.Contains(body, `action="/console/agents/support-bot/channels"`) {
		t.Errorf("link form action missing; body:\n%s", body)
	}
	// The picker must offer the unlinked integration and omit the already-linked one.
	if !strings.Contains(body, `value="`+unlinked.ID+`"`) {
		t.Errorf("unlinked integration not offered in picker; body:\n%s", body)
	}
	if strings.Contains(body, `value="`+linked.ID+`"`) {
		t.Errorf("already-linked integration must not be offered in picker; body:\n%s", body)
	}
}

func TestAgents_ChannelsTab_LinkButtonHiddenWhenReadOnly(t *testing.T) {
	tc := newTestConsole(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/agents/support-bot/tabs/channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "Link channel") {
		t.Errorf("link control rendered in read-only console")
	}
}
