package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// startAgentServer boots the HTTP API over a fresh temp SQLite store with the
// agents surface mounted (no engine: agent CRUD never touches dagnats) and
// returns the server plus a bootstrapped API key header.
func startAgentServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	trg := trigger.New(aud, st, nil)
	inbox := inboxapi.New(st)
	ag := agentsapi.New(agentstore.New(st))

	key, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, ag, nil, nil, nil, nil, nil))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key
}

// TestAgents_AuthGuard proves every agents route sits behind the identity
// middleware: no Authorization header answers 401.
func TestAgents_AuthGuard(t *testing.T) {
	srv, _ := startAgentServer(t)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/agents"},
		{http.MethodGet, "/v1/agents"},
		{http.MethodGet, "/v1/agents/support-bot"},
		{http.MethodPatch, "/v1/agents/support-bot"},
		{http.MethodDelete, "/v1/agents/support-bot"},
		{http.MethodPost, "/v1/agents/support-bot/integrations"},
		{http.MethodGet, "/v1/agents/support-bot/integrations"},
		{http.MethodDelete, "/v1/agents/support-bot/integrations/int-slack"},
	} {
		req, err := http.NewRequest(route.method, srv.URL+route.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", route.method, route.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without key: status=%d, want 401", route.method, route.path, resp.StatusCode)
		}
	}
}

// TestAgents_DuplicateIdentifierConflict pins the duplicate-create contract:
// re-POSTing an existing identifier answers 409 with a clean message, not a
// leaked SQLite constraint error.
func TestAgents_DuplicateIdentifierConflict(t *testing.T) {
	srv, authHeader := startAgentServer(t)

	createBody := `{"identifier":"support-bot","runtime":"mesh"}`
	if status, body := doPost(t, srv.URL+"/v1/agents", authHeader, createBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/agents: status=%d body=%s", status, body)
	}
	status, body := doPost(t, srv.URL+"/v1/agents", authHeader, createBody)
	if status != http.StatusConflict {
		t.Fatalf("duplicate POST: status=%d body=%s, want 409", status, body)
	}
	if strings.TrimSpace(body) != "agent identifier already exists" {
		t.Fatalf("duplicate POST body = %q, want %q", strings.TrimSpace(body), "agent identifier already exists")
	}
}

// TestAgents_CRUDLifecycle drives an agent through create -> get -> list ->
// patch -> delete against the real HTTP surface and temp SQLite.
func TestAgents_CRUDLifecycle(t *testing.T) {
	srv, authHeader := startAgentServer(t)

	createBody := `{"identifier":"support-bot","name":"Support Bot","runtime":"mesh","meshSubject":"agents.support.prompt","welcomeMessage":"hi"}`
	status, body := doPost(t, srv.URL+"/v1/agents", authHeader, createBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/agents: status=%d body=%s", status, body)
	}
	var created agentdomain.Agent
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode created agent: %v (body=%s)", err, body)
	}
	if created.ID == "" || created.Identifier != "support-bot" || created.Runtime != "mesh" || !created.Active {
		t.Fatalf("unexpected created agent: %+v", created)
	}

	status, body = doGet(t, srv.URL+"/v1/agents/support-bot", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/agents/support-bot: status=%d body=%s", status, body)
	}
	var got agentdomain.Agent
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if got.ID != created.ID || got.MeshSubject != "agents.support.prompt" || got.WelcomeMessage != "hi" {
		t.Fatalf("GET mismatch: %+v", got)
	}

	status, body = doGet(t, srv.URL+"/v1/agents", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/agents: status=%d body=%s", status, body)
	}
	var list []agentdomain.Agent
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode agent list: %v", err)
	}
	if len(list) != 1 || list[0].Identifier != "support-bot" {
		t.Fatalf("expected [support-bot], got %+v", list)
	}

	patchBody := `{"name":"Support Bot v2","runtime":"bridge","bridgeUrl":"https://bot.example.com/turn","active":false}`
	status, body = doPatch(t, srv.URL+"/v1/agents/support-bot", authHeader, patchBody)
	if status != http.StatusOK {
		t.Fatalf("PATCH /v1/agents/support-bot: status=%d body=%s", status, body)
	}
	var patched agentdomain.Agent
	if err := json.Unmarshal([]byte(body), &patched); err != nil {
		t.Fatalf("decode patched agent: %v", err)
	}
	if patched.Name != "Support Bot v2" || patched.Runtime != "bridge" || patched.BridgeURL != "https://bot.example.com/turn" || patched.Active {
		t.Fatalf("patch not applied: %+v", patched)
	}
	if patched.MeshSubject != "agents.support.prompt" {
		t.Fatalf("patch must not clear untouched fields: %+v", patched)
	}
	if patched.ID != created.ID {
		t.Fatalf("patch changed id: %s -> %s", created.ID, patched.ID)
	}

	status, body = doDelete(t, srv.URL+"/v1/agents/support-bot", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/agents/support-bot: status=%d body=%s", status, body)
	}
	if status, _ = doGet(t, srv.URL+"/v1/agents/support-bot", authHeader); status != http.StatusNotFound {
		t.Fatalf("GET after delete: status=%d, want 404", status)
	}
	if status, _ = doDelete(t, srv.URL+"/v1/agents/support-bot", authHeader); status != http.StatusNotFound {
		t.Fatalf("second DELETE: status=%d, want 404", status)
	}
}

// TestAgents_RuntimeEnumMessage pins the 422 contract: an invalid runtime must
// enumerate the allowed values in the error message, on create and on patch.
func TestAgents_RuntimeEnumMessage(t *testing.T) {
	srv, authHeader := startAgentServer(t)

	const wantMsg = "runtime must be one of: mesh, bridge, managed"

	status, body := doPost(t, srv.URL+"/v1/agents", authHeader, `{"identifier":"bad-bot","runtime":"warp"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("POST with bad runtime: status=%d body=%s, want 422", status, body)
	}
	if strings.TrimSpace(body) != wantMsg {
		t.Fatalf("POST 422 body = %q, want %q", strings.TrimSpace(body), wantMsg)
	}

	// Missing runtime is the same contract as a wrong one.
	status, body = doPost(t, srv.URL+"/v1/agents", authHeader, `{"identifier":"bad-bot"}`)
	if status != http.StatusUnprocessableEntity || strings.TrimSpace(body) != wantMsg {
		t.Fatalf("POST without runtime: status=%d body=%q, want 422 %q", status, strings.TrimSpace(body), wantMsg)
	}

	if status, body = doPost(t, srv.URL+"/v1/agents", authHeader, `{"identifier":"good-bot","runtime":"managed"}`); status != http.StatusCreated {
		t.Fatalf("POST valid agent: status=%d body=%s", status, body)
	}
	status, body = doPatch(t, srv.URL+"/v1/agents/good-bot", authHeader, `{"runtime":"warp"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH with bad runtime: status=%d body=%s, want 422", status, body)
	}
	if strings.TrimSpace(body) != wantMsg {
		t.Fatalf("PATCH 422 body = %q, want %q", strings.TrimSpace(body), wantMsg)
	}
}

// TestAgents_LinkUnlinkIntegrations drives the agent_links surface: link a
// channel integration, list it, re-link to update config, unlink.
func TestAgents_LinkUnlinkIntegrations(t *testing.T) {
	srv, authHeader := startAgentServer(t)

	if status, body := doPost(t, srv.URL+"/v1/agents", authHeader, `{"identifier":"support-bot","runtime":"mesh"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/agents: status=%d body=%s", status, body)
	}

	status, body := doPost(t, srv.URL+"/v1/agents/support-bot/integrations", authHeader, `{"integrationId":"int-slack","config":{"channel":"#support"}}`)
	if status != http.StatusCreated {
		t.Fatalf("POST integrations: status=%d body=%s", status, body)
	}
	var link agentdomain.AgentLink
	if err := json.Unmarshal([]byte(body), &link); err != nil {
		t.Fatalf("decode link: %v (body=%s)", err, body)
	}
	if link.ID == "" || link.IntegrationID != "int-slack" || link.ConnectedAt == "" {
		t.Fatalf("unexpected link: %+v", link)
	}

	status, body = doGet(t, srv.URL+"/v1/agents/support-bot/integrations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET integrations: status=%d body=%s", status, body)
	}
	var links []agentdomain.AgentLink
	if err := json.Unmarshal([]byte(body), &links); err != nil {
		t.Fatalf("decode links: %v", err)
	}
	if len(links) != 1 || links[0].ID != link.ID || string(links[0].Config) != `{"channel":"#support"}` {
		t.Fatalf("expected one link with config, got %+v", links)
	}

	// Linking to an unknown agent answers 404.
	if status, _ = doPost(t, srv.URL+"/v1/agents/ghost/integrations", authHeader, `{"integrationId":"int-slack"}`); status != http.StatusNotFound {
		t.Fatalf("POST integrations on ghost agent: status=%d, want 404", status)
	}

	status, body = doDelete(t, srv.URL+"/v1/agents/support-bot/integrations/int-slack", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE integration link: status=%d body=%s", status, body)
	}
	status, body = doGet(t, srv.URL+"/v1/agents/support-bot/integrations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET integrations after unlink: status=%d body=%s", status, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("expected empty link list after unlink, got %s", body)
	}
	if status, _ = doDelete(t, srv.URL+"/v1/agents/support-bot/integrations/int-slack", authHeader); status != http.StatusNotFound {
		t.Fatalf("second unlink: status=%d, want 404", status)
	}
}

// TestAgents_ListConversations drives the GET /v1/agents/{identifier}/conversations
// route: 404 on unknown agent, empty list, pagination via before cursor.
func TestAgents_ListConversations(t *testing.T) {
	srv, authHeader := startAgentServer(t)

	// Unknown agent → 404.
	if status, _ := doGet(t, srv.URL+"/v1/agents/ghost/conversations", authHeader); status != http.StatusNotFound {
		t.Fatalf("unknown agent: status=%d, want 404", status)
	}

	// Create an agent.
	status, body := doPost(t, srv.URL+"/v1/agents", authHeader, `{"identifier":"chat-bot","runtime":"mesh"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/agents: status=%d body=%s", status, body)
	}
	var agent agentdomain.Agent
	if err := json.Unmarshal([]byte(body), &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}

	// No conversations yet → empty list (not null).
	status, body = doGet(t, srv.URL+"/v1/agents/chat-bot/conversations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET conversations: status=%d body=%s", status, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("expected empty list, got %s", body)
	}

	// Auth guard: no header → 401.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/agents/chat-bot/conversations", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauth request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth: status=%d, want 401", resp.StatusCode)
	}
}

// TestAgents_ChannelSetup drives the
// GET /v1/agents/{identifier}/channels/{integrationID}/setup route.
func TestAgents_ChannelSetup(t *testing.T) {
	srv, authHeader := startAgentServer(t)

	// Create an agent.
	if status, body := doPost(t, srv.URL+"/v1/agents", authHeader, `{"identifier":"tg-bot","runtime":"bridge","bridgeUrl":"https://bot.example.com/turn"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/agents: status=%d body=%s", status, body)
	}

	// Unknown agent → 404.
	if status, _ := doGet(t, srv.URL+"/v1/agents/ghost/channels/int-tg/setup", authHeader); status != http.StatusNotFound {
		t.Fatalf("unknown agent: status=%d, want 404", status)
	}

	// No channel link yet → 404.
	if status, _ := doGet(t, srv.URL+"/v1/agents/tg-bot/channels/int-tg/setup", authHeader); status != http.StatusNotFound {
		t.Fatalf("unlinked channel: status=%d, want 404", status)
	}

	// Link the integration.
	if status, body := doPost(t, srv.URL+"/v1/agents/tg-bot/integrations", authHeader, `{"integrationId":"int-tg"}`); status != http.StatusCreated {
		t.Fatalf("link integration: status=%d body=%s", status, body)
	}

	// Now setup returns the webhook URL (base_url placeholder since no baseURL configured).
	status, body := doGet(t, srv.URL+"/v1/agents/tg-bot/channels/int-tg/setup", authHeader)
	if status != http.StatusOK {
		t.Fatalf("setup: status=%d body=%s", status, body)
	}
	var resp struct {
		WebhookURL string `json:"webhookUrl"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode setup response: %v (body=%s)", err, body)
	}
	if resp.WebhookURL == "" {
		t.Fatalf("webhookUrl is empty")
	}
	// Without a configured baseURL the response uses the placeholder pattern.
	if !strings.Contains(resp.WebhookURL, "int-tg") {
		t.Fatalf("webhookUrl %q must contain the integrationID 'int-tg'", resp.WebhookURL)
	}

	// Unlinked integration returns 404 even when agent exists.
	if status, _ := doGet(t, srv.URL+"/v1/agents/tg-bot/channels/other-int/setup", authHeader); status != http.StatusNotFound {
		t.Fatalf("unlinked other: status=%d, want 404", status)
	}

	// Auth guard: no header → 401.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/agents/tg-bot/channels/int-tg/setup", nil)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauth request: %v", err)
	}
	_ = got.Body.Close()
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth: status=%d, want 401", got.StatusCode)
	}
}

// doPatch issues an authenticated JSON PATCH and returns the status and body.
func doPatch(t *testing.T, url, authHeader, body string) (int, string) {
	t.Helper()
	return doMethod(t, http.MethodPatch, url, authHeader, body)
}

// doDelete issues an authenticated DELETE and returns the status and body.
func doDelete(t *testing.T, url, authHeader string) (int, string) {
	t.Helper()
	return doMethod(t, http.MethodDelete, url, authHeader, "")
}

// doMethod issues an authenticated request with an optional JSON body and
// returns the status and body.
func doMethod(t *testing.T, method, url, authHeader, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", authHeader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
