package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
)

// seedBasicAgent seeds one mesh agent with a name and returns it.
func seedBasicAgent(t *testing.T, tc *testConsole, identifier, name string) agentdomain.Agent {
	t.Helper()
	return tc.seedAgent(t, agentdomain.Agent{
		Identifier:  identifier,
		Name:        name,
		Runtime:     agentdomain.RuntimeMesh,
		Active:      true,
		MeshSubject: "agents." + identifier + ".v1",
	})
}

func TestAgents_PageRenderWithSeededData(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	rr := tc.get(t, "/console/agents", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "support-bot") {
		t.Errorf("page missing agent identifier; body:\n%s", body)
	}
	if !strings.Contains(body, "Support Bot") {
		t.Errorf("page missing agent name")
	}
}

func TestAgents_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/agents", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No agents yet") {
		t.Errorf("expected empty state; body:\n%s", rr.Body.String())
	}
}

func TestAgents_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/agents", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect to login", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/console/login") {
		t.Errorf("Location = %q, want login redirect", loc)
	}
}

func TestAgents_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/agents", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/agents"`) {
		t.Errorf("nav entry for agents not present")
	}
	// The list page renders section "agents", so the sidebar nav link is
	// active (chassis idiom: "console-nav-link is-active" + aria-current).
	if !strings.Contains(body, `console-nav-link is-active`) {
		t.Errorf("expected an active nav entry on the agents page")
	}
}

func TestAgents_CreateFormRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/agents/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/console/agents"`) {
		t.Errorf("create form action missing")
	}
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Errorf("create form missing csrf token")
	}
	if !strings.Contains(body, `name="identifier"`) {
		t.Errorf("create form missing identifier field")
	}
}

func TestAgents_Create_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}, "identifier": {"x"}, "runtime": {"mesh"}}
	rr := tc.postForm(t, "/console/agents", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 in read-only", rr.Code)
	}
}

func TestAgents_Create_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := url.Values{
		"csrf_token": {csrf},
		"identifier": {"new-agent"},
		"name":       {"New Agent"},
		"runtime":    {"mesh"},
	}
	rr := tc.postForm(t, "/console/agents", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/console/agents/new-agent" {
		t.Errorf("Location = %q, want /console/agents/new-agent", loc)
	}
	// Verify persisted.
	got, err := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "new-agent")
	if err != nil {
		t.Fatalf("agent not persisted: %v", err)
	}
	if got.Name != "New Agent" || got.Runtime != "mesh" || !got.Active {
		t.Errorf("persisted agent = %+v, want name/runtime/active set", got)
	}
}

func TestAgents_Create_InvalidRuntimeRedirectsWithError(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}, "identifier": {"x"}, "runtime": {"bogus"}}
	rr := tc.postForm(t, "/console/agents", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error redirect", loc)
	}
}

// TestAgents_CreateFormRuntimeOptions_OnlySupported asserts the create form's
// runtime <select> offers exactly SupportedRuntimes — no unimplemented
// bridge/managed options that would silently dead-letter every turn (#103).
func TestAgents_CreateFormRuntimeOptions_OnlySupported(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/agents/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<option value="mesh">mesh</option>`) {
		t.Errorf("create form missing supported mesh option; body:\n%s", body)
	}
	if strings.Contains(body, `<option value="bridge">`) {
		t.Errorf("create form must NOT offer unimplemented bridge runtime")
	}
	if strings.Contains(body, `<option value="managed">`) {
		t.Errorf("create form must NOT offer unimplemented managed runtime")
	}
}

// TestAgents_Create_UnsupportedRuntimeRejected asserts a direct POST with a
// defined-but-unimplemented runtime (bridge/managed) is rejected with an inline
// error and persists nothing — defense in depth behind the form (#103).
func TestAgents_Create_UnsupportedRuntimeRejected(t *testing.T) {
	for _, runtime := range []string{"bridge", "managed"} {
		t.Run(runtime, func(t *testing.T) {
			tc := newTestConsole(t)
			cookie, csrf := tc.login(t)

			form := url.Values{"csrf_token": {csrf}, "identifier": {"dead-" + runtime}, "runtime": {runtime}}
			rr := tc.postForm(t, "/console/agents", form, cookie)
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303", rr.Code)
			}
			if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
				t.Errorf("Location = %q, want error redirect", loc)
			}
			if _, err := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "dead-"+runtime); err == nil {
				t.Errorf("agent with unsupported runtime %q was persisted", runtime)
			}
		})
	}
}

// TestAgents_Save_UnsupportedRuntimeRejected guards the save handler: an agent
// whose stored runtime is unsupported (a row created before the guard, or via a
// crafted POST) must not be re-saved through the edit form (#103).
func TestAgents_Save_UnsupportedRuntimeRejected(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	// Seed directly through the store, bypassing the create guard, to simulate
	// a legacy dead agent.
	tc.seedAgent(t, agentdomain.Agent{
		Identifier: "legacy-bridge",
		Name:       "Legacy",
		Runtime:    agentdomain.RuntimeBridge,
		Active:     true,
		BridgeURL:  "https://x.example.com/dispatch",
	})

	form := url.Values{"csrf_token": {csrf}, "name": {"Renamed"}}
	rr := tc.postForm(t, "/console/agents/legacy-bridge/save", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error redirect", loc)
	}
	got, err := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "legacy-bridge")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name == "Renamed" {
		t.Errorf("save persisted despite unsupported runtime")
	}
}

func TestAgents_Detail_Overview(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	rr := tc.get(t, "/console/agents/support-bot", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Overview") {
		t.Errorf("overview tab not rendered")
	}
	if !strings.Contains(body, `action="/console/agents/support-bot/save"`) {
		t.Errorf("edit form not rendered on overview")
	}
}

func TestAgents_Detail_ChannelsTab(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	rr := tc.get(t, "/console/agents/support-bot/tabs/channels", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No channels linked") {
		t.Errorf("channels empty state not rendered; body:\n%s", body)
	}
}

func TestAgents_Detail_ConversationsTab(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	a := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	tc.seedConversation(t, agentdomain.Conversation{
		AgentID:          a.ID,
		PlatformThreadID: "thread-1",
		SubscriberID:     "sub-1",
		Status:           "open",
	})

	rr := tc.get(t, "/console/agents/support-bot/tabs/conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sub-1") {
		t.Errorf("conversation row not rendered; body:\n%s", body)
	}
	if !strings.Contains(body, "/console/agents/support-bot/conversations/") {
		t.Errorf("open-conversation link missing")
	}
}

func TestAgents_Detail_NotFound(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/agents/does-not-exist", cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAgents_Save_Persists(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	form := url.Values{
		"csrf_token":     {csrf},
		"name":           {"Renamed Bot"},
		"description":    {"new desc"},
		"welcomeMessage": {"hello"},
		"meshSubject":    {"agents.support-bot.v2"},
	}
	rr := tc.postForm(t, "/console/agents/support-bot/save", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rr.Code, rr.Body.String())
	}
	got, err := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "support-bot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Renamed Bot" || got.Description != "new desc" || got.WelcomeMessage != "hello" {
		t.Errorf("save did not persist: %+v", got)
	}
}

func TestAgents_Save_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}, "name": {"x"}}
	rr := tc.postForm(t, "/console/agents/support-bot/save", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestAgents_Delete_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	form := url.Values{"csrf_token": {csrf}}
	rr := tc.postForm(t, "/console/agents/support-bot/delete", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/console/agents" {
		t.Errorf("Location = %q, want /console/agents", loc)
	}
	if _, err := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "support-bot"); err == nil {
		t.Errorf("agent still present after delete")
	}
}

func TestAgents_Delete_TwoStepConfirm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")

	// The list fragment with ?confirmDelete=<identifier> renders the confirm
	// state (a real POST form), not a native confirm() dialog.
	rr := tc.get(t, "/console/api/fragments/agents-list?confirmDelete=support-bot", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Confirm delete") {
		t.Errorf("confirm state not rendered; body:\n%s", body)
	}
	if !strings.Contains(body, `action="/console/agents/support-bot/delete"`) {
		t.Errorf("confirm form action missing")
	}
}

func TestAgents_Delete_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}}
	rr := tc.postForm(t, "/console/agents/support-bot/delete", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestAgents_ToggleActive_Redirect(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot") // starts active

	form := url.Values{"csrf_token": {csrf}}
	rr := tc.postForm(t, "/console/agents/support-bot/activate", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	got, _ := tc.agentStore.Agents().GetByIdentifier(testBackground(), tc.scope, "support-bot")
	if got.Active {
		t.Errorf("expected agent deactivated after toggle")
	}
}

func TestAgents_ToggleActive_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}}
	rr := tc.postForm(t, "/console/agents/support-bot/activate", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestAgents_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	// No cookie → all agent routes gate to login.
	for _, path := range []string{
		"/console/agents",
		"/console/agents/new",
		"/console/agents/x",
		"/console/agents/x/tabs/channels",
		"/console/api/fragments/agents-list",
	} {
		rr := tc.get(t, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want 303 redirect to login", path, rr.Code)
		}
	}
}

func TestAgents_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	// Agent seeded under a different tenant must not surface.
	alt := tc.altScope(t)
	tc.seedAgentInScope(t, alt, agentdomain.Agent{
		Identifier: "other-tenant-bot",
		Name:       "Other",
		Runtime:    agentdomain.RuntimeMesh,
		Active:     true,
	})

	rr := tc.get(t, "/console/agents", cookie)
	if strings.Contains(rr.Body.String(), "other-tenant-bot") {
		t.Errorf("cross-tenant agent leaked into list")
	}
	// And the detail page 404s for the other tenant's agent.
	rr = tc.get(t, "/console/agents/other-tenant-bot", cookie)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant detail status = %d, want 404", rr.Code)
	}
}

// ─── conversation view ────────────────────────────────────────────────────────

func seedConvWithTurn(t *testing.T, tc *testConsole) (agentdomain.Agent, agentdomain.Conversation) {
	t.Helper()
	a := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	conv := tc.seedConversation(t, agentdomain.Conversation{
		AgentID:          a.ID,
		PlatformThreadID: "thread-1",
		SubscriberID:     "sub-1",
		Status:           "open",
	})
	content, _ := json.Marshal(map[string][]map[string]string{"parts": {{"text": "Hello there"}}})
	tc.seedActivity(t, agentdomain.Activity{
		ConversationID: conv.ID,
		Role:           "user",
		Content:        content,
	})
	return a, conv
}

func TestConversation_PageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	_, conv := seedConvWithTurn(t, tc)

	rr := tc.get(t, "/console/agents/support-bot/conversations/"+conv.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, conv.ID) {
		t.Errorf("conversation id not rendered")
	}
	if !strings.Contains(body, "thread-1") {
		t.Errorf("thread id not rendered")
	}
}

func TestConversation_TranscriptRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	_, conv := seedConvWithTurn(t, tc)

	rr := tc.get(t, "/console/agents/support-bot/conversations/"+conv.ID, cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "Hello there") {
		t.Errorf("decoded transcript text not rendered; body:\n%s", body)
	}
	if !strings.Contains(body, "agent-turn--user") {
		t.Errorf("user turn bubble class not rendered")
	}
}

func TestConversation_NotFound(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedBasicAgent(t, tc, "support-bot", "Support Bot")
	rr := tc.get(t, "/console/agents/support-bot/conversations/nope", cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestConversation_ComposePostRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	_, conv := seedConvWithTurn(t, tc)

	form := url.Values{"csrf_token": {csrf}, "text": {"a reply"}}
	rr := tc.postForm(t, "/console/agents/support-bot/conversations/"+conv.ID+"/compose", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rr.Code, rr.Body.String())
	}
	want := "/console/agents/support-bot/conversations/" + conv.ID
	if loc := rr.Header().Get("Location"); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
	if len(tc.composer.calls) != 1 {
		t.Fatalf("composer called %d times, want 1", len(tc.composer.calls))
	}
	call := tc.composer.calls[0]
	if call.agentIdentifier != "support-bot" || call.text != "a reply" || call.conversationID != conv.PlatformThreadID {
		t.Errorf("compose call = %+v, want agent=support-bot text='a reply' conv=%s", call, conv.PlatformThreadID)
	}
	if call.subscriberID != "operator" {
		t.Errorf("compose subscriberID = %q, want operator", call.subscriberID)
	}
}

func TestConversation_ComposeReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	_, conv := seedConvWithTurn(t, tc)
	tc.deps.ReadOnly = true
	tc.c.deps.ReadOnly = true
	cookie, csrf := tc.login(t)

	form := url.Values{"csrf_token": {csrf}, "text": {"a reply"}}
	rr := tc.postForm(t, "/console/agents/support-bot/conversations/"+conv.ID+"/compose", form, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if len(tc.composer.calls) != 0 {
		t.Errorf("composer should not be called in read-only mode")
	}
}

func TestConversation_ComposeEmptyTextRedirectsNoCall(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	_, conv := seedConvWithTurn(t, tc)

	form := url.Values{"csrf_token": {csrf}, "text": {"   "}}
	rr := tc.postForm(t, "/console/agents/support-bot/conversations/"+conv.ID+"/compose", form, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if len(tc.composer.calls) != 0 {
		t.Errorf("composer should not be called for empty text")
	}
}
