// conversations_test.go exercises the W4-A5 global conversations console page
// and its Datastar fragment endpoint against the real agents store.
package console

import (
	"net/http"
	"strings"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
)

// ─── helper ───────────────────────────────────────────────────────────────────

// seedConvWithActivity seeds one agent + one conversation + one activity turn
// so that ListConversations returns a row (requires at least one activity row).
func seedGlobalConvRow(t *testing.T, tc *testConsole, agentIdentifier, agentName string) agentdomain.Agent {
	t.Helper()
	agent := tc.seedAgent(t, agentdomain.Agent{
		Identifier: agentIdentifier,
		Name:       agentName,
		Runtime:    agentdomain.RuntimeMesh,
		Active:     true,
	})
	conv := tc.seedConversation(t, agentdomain.Conversation{
		AgentID:          agent.ID,
		PlatformThreadID: "thread-" + agentIdentifier,
		SubscriberID:     "sub-" + agentIdentifier,
	})
	tc.seedActivity(t, agentdomain.Activity{
		ConversationID: conv.ID,
		Role:           "user",
	})
	return agent
}

// ─── page render tests ────────────────────────────────────────────────────────

func TestConversations_PageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Conversations") {
		t.Errorf("page missing 'Conversations' heading; body:\n%s", body)
	}
}

func TestConversations_PageShowsRows(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedGlobalConvRow(t, tc, "test-agent", "Test Agent")

	rr := tc.get(t, "/console/conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "test-agent") {
		t.Errorf("page missing agent identifier 'test-agent'; body:\n%s", body)
	}
	if !strings.Contains(body, "Test Agent") {
		t.Errorf("page missing agent name 'Test Agent'")
	}
}

func TestConversations_PageEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No conversations yet") {
		t.Errorf("expected empty-state text; body:\n%s", rr.Body.String())
	}
}

func TestConversations_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/conversations", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect to login", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/console/login") {
		t.Errorf("Location = %q, want login redirect", loc)
	}
}

func TestConversations_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/conversations", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, `href="/console/conversations"`) {
		t.Errorf("sidebar nav entry for Conversations not present; body:\n%s", body)
	}
	// Conversations page is section "conversations" — its nav link is active.
	if !strings.Contains(body, `console-nav-link is-active`) {
		t.Errorf("expected active nav entry on the conversations page")
	}
}

func TestConversations_RowDeepLinksToPerAgentView(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	agent := seedGlobalConvRow(t, tc, "link-agent", "Link Agent")

	rr := tc.get(t, "/console/conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Row must link to /console/agents/{identifier}/conversations/{id}
	if !strings.Contains(body, "/console/agents/"+agent.Identifier+"/conversations/") {
		t.Errorf("deep-link to per-agent conversation view missing; body:\n%s", body)
	}
}

// ─── fragment tests ────────────────────────────────────────────────────────────

func TestConversations_FragmentRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	seedGlobalConvRow(t, tc, "frag-agent", "Frag Agent")

	rr := tc.get(t, "/console/api/fragments/global-conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200; body:\n%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "frag-agent") {
		t.Errorf("fragment missing agent identifier; body:\n%s", body)
	}
}

func TestConversations_FragmentEmptyState(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/global-conversations", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No conversations yet") {
		t.Errorf("expected empty-state text in fragment; body:\n%s", rr.Body.String())
	}
}

func TestConversations_FragmentUnauthRedirects(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/api/fragments/global-conversations", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("unauth fragment: status = %d, want 303", rr.Code)
	}
}

func TestConversations_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	// Both the page and the fragment must redirect unauthenticated requests.
	for _, path := range []string{
		"/console/conversations",
		"/console/api/fragments/global-conversations",
	} {
		rr := tc.get(t, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("GET %s unauthenticated: status=%d, want 303", path, rr.Code)
		}
	}
}

func TestConversations_ScopeIsolation(t *testing.T) {
	// Two different testConsole instances share no data (each has its own
	// temp SQLite file). Conversations seeded in tc1 must never appear in tc2.
	tc1 := newTestConsole(t)
	tc2 := newTestConsole(t)

	cookie1, _ := tc1.login(t)
	cookie2, _ := tc2.login(t)

	seedGlobalConvRow(t, tc1, "scope-agent", "Scope Agent")

	rr1 := tc1.get(t, "/console/conversations", cookie1)
	if !strings.Contains(rr1.Body.String(), "scope-agent") {
		t.Errorf("tc1 should see scope-agent; body:\n%s", rr1.Body.String())
	}

	rr2 := tc2.get(t, "/console/conversations", cookie2)
	if strings.Contains(rr2.Body.String(), "scope-agent") {
		t.Errorf("tc2 must NOT see tc1's scope-agent")
	}
	if !strings.Contains(rr2.Body.String(), "No conversations yet") {
		t.Errorf("tc2 should show empty state; body:\n%s", rr2.Body.String())
	}
}
