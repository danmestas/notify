package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

func startManagedServer(t *testing.T, box *secretbox.Box, opts ...func(*agentsapi.Service) *agentsapi.Service) (*httptest.Server, string, *agentstore.Store, domain.Scope) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	ags := agentstore.New(st)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	ag := agentsapi.New(ags)
	if box != nil {
		ag = ag.WithSecretBox(box)
	}
	for _, o := range opts {
		ag = o(ag)
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st), Agents: ag}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, ags, scope
}

// TestManagedAgent_KeyIsSealedAndNeverReturned: a managed agent's API key is
// stored sealed and never appears in a response; a patch that omits it keeps
// it, an empty one clears it, and a server with no master key refuses one.
func TestManagedAgent_KeyIsSealedAndNeverReturned(t *testing.T) {
	box, err := secretbox.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv, auth, ags, scope := startManagedServer(t, box)
	body := `{"identifier":"drafter","name":"Drafter","runtime":"managed","managed":{"provider":"openai","baseUrl":"http://localhost:11434/v1","model":"llama3","apiKey":"sk-secret"}}`
	status, resp := doPost(t, srv.URL+"/v1/agents", auth, body)
	if status != http.StatusCreated || strings.Contains(resp, "sk-secret") || !strings.Contains(resp, `"apiKeySet":true`) {
		t.Fatalf("create: %d %s", status, resp)
	}
	stored := func() map[string]any {
		a, err := ags.Agents().GetByIdentifier(context.Background(), scope, "drafter")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(a.Managed, &m)
		return m
	}
	k, _ := stored()["apiKey"].(string)
	if !secretbox.IsSealed(k) {
		t.Fatalf("stored key %q is not sealed", k)
	}
	if opened, err := box.Open(k); err != nil || opened != "sk-secret" {
		t.Fatalf("stored key opens to %q, %v", opened, err)
	}

	patch := func(b string) (int, string) {
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/drafter", strings.NewReader(b))
		req.Header.Set("Authorization", auth)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		return r.StatusCode, buf.String()
	}
	if s, resp := patch(`{"managed":{"provider":"openai","baseUrl":"http://localhost:11434/v1","model":"llama3.1","systemPrompt":"Be brief."}}`); s != http.StatusOK || !strings.Contains(resp, `"apiKeySet":true`) {
		t.Fatalf("patch without key: %d %s", s, resp)
	}
	if k2, _ := stored()["apiKey"].(string); k2 != k {
		t.Fatal("a patch that omitted the key changed it")
	}
	if s, resp := patch(`{"managed":{"provider":"openai","baseUrl":"http://localhost:11434/v1","model":"llama3.1","apiKey":""}}`); s != http.StatusOK || strings.Contains(resp, "apiKeySet\":true") {
		t.Fatalf("clearing the key: %d %s", s, resp)
	}
	if _, has := stored()["apiKey"]; has {
		t.Fatal("an empty key did not clear the stored one")
	}
	status, resp = doGet(t, srv.URL+"/v1/agents/drafter", auth)
	if status != http.StatusOK || strings.Contains(resp, "apiKey\":") {
		t.Fatalf("get: %d %s", status, resp)
	}

	noBox, auth2, _, _ := startManagedServer(t, nil)
	if status, resp := doPost(t, noBox.URL+"/v1/agents", auth2, body); status != http.StatusUnprocessableEntity {
		t.Fatalf("no master key: %d %s, want 422", status, resp)
	}
}

// TestManagedAgent_SettingsAreCheckedOnSave: settings a managed agent could
// not run with are refused when saved, not discovered on its first turn; any
// named provider with a known endpoint, or with its own baseUrl, is accepted.
func TestManagedAgent_SettingsAreCheckedOnSave(t *testing.T) {
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	srv, auth, _, _ := startManagedServer(t, box)
	post := func(id, managed string) int {
		s, _ := doPost(t, srv.URL+"/v1/agents", auth, `{"identifier":"`+id+`","runtime":"managed","managed":`+managed+`}`)
		return s
	}
	for id, m := range map[string]string{
		"no-model":         `{"provider":"openrouter"}`,
		"unknown-provider": `{"provider":"bard","model":"m"}`,
		"too-many-tokens":  `{"provider":"groq","model":"m","maxTokens":100000}`,
	} {
		if s := post(id, m); s != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", id, s)
		}
	}
	for id, m := range map[string]string{
		"openrouter":  `{"provider":"openrouter","model":"anthropic/claude-sonnet-4.5"}`,
		"own-vllm":    `{"provider":"my-vllm","baseUrl":"http://10.0.0.5:8000/v1","model":"m"}`,
		"unset-later": `null`,
	} {
		if s := post(id, m); s != http.StatusCreated {
			t.Errorf("%s: status %d, want 201", id, s)
		}
	}
}

// TestManagedAgent_MCPServerSecretsAreSealed: each MCP server's auth secret
// is sealed like the API key and shown only as authSecretSet; a patch that
// omits it keeps the stored one for the same server name, an empty one clears
// it, and a server with no master key refuses one.
func TestManagedAgent_MCPServerSecretsAreSealed(t *testing.T) {
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	srv, auth, ags, scope := startManagedServer(t, box)
	body := `{"identifier":"vaulted","runtime":"managed","managed":{"provider":"openrouter","model":"m","mcpServers":[{"name":"vault","url":"https://vault.example/mcp","authSecret":"Bearer vault-secret"},{"name":"open","url":"https://open.example/mcp"}]}}`
	status, resp := doPost(t, srv.URL+"/v1/agents", auth, body)
	if status != http.StatusCreated || strings.Contains(resp, "vault-secret") || strings.Contains(resp, `"authSecret"`) ||
		!strings.Contains(resp, `{"authSecretSet":true,"name":"vault","url":"https://vault.example/mcp"}`) ||
		!strings.Contains(resp, `{"name":"open","url":"https://open.example/mcp"}`) {
		t.Fatalf("create: %d %s", status, resp)
	}
	secret := func() (string, bool) {
		a, err := ags.Agents().GetByIdentifier(context.Background(), scope, "vaulted")
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			MCPServers []map[string]any `json:"mcpServers"`
		}
		_ = json.Unmarshal(a.Managed, &m)
		s, ok := m.MCPServers[0]["authSecret"].(string)
		return s, ok
	}
	sealed, _ := secret()
	if opened, err := box.Open(sealed); !secretbox.IsSealed(sealed) || err != nil || opened != "Bearer vault-secret" {
		t.Fatalf("stored secret %q opens to %q, %v", sealed, opened, err)
	}
	patch := func(b string) (int, string) {
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/vaulted", strings.NewReader(b))
		req.Header.Set("Authorization", auth)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		return r.StatusCode, buf.String()
	}
	// Same url and header: the stored secret is kept.
	if s, resp := patch(`{"managed":{"provider":"openrouter","model":"m2","mcpServers":[{"name":"vault","url":"https://vault.example/mcp"}]}}`); s != http.StatusOK || !strings.Contains(resp, `"authSecretSet":true`) {
		t.Fatalf("patch without secret: %d %s", s, resp)
	}
	if kept, _ := secret(); kept != sealed {
		t.Fatal("a patch that omitted the secret changed it")
	}
	// A new destination must not inherit the secret: send it again.
	for name, servers := range map[string]string{
		"url":    `[{"name":"vault","url":"https://attacker.example/mcp"}]`,
		"header": `[{"name":"vault","url":"https://vault.example/mcp","authHeader":"X-Api-Key"}]`,
	} {
		if s, resp := patch(`{"managed":{"provider":"openrouter","model":"m2","mcpServers":` + servers + `}}`); s != http.StatusUnprocessableEntity || !strings.Contains(resp, "authSecret") {
			t.Fatalf("changed %s without secret: %d %s, want 422", name, s, resp)
		}
	}
	if kept, _ := secret(); kept != sealed {
		t.Fatal("a refused patch changed the secret")
	}
	if s, resp := patch(`{"managed":{"provider":"openrouter","model":"m2","mcpServers":[{"name":"vault","url":"https://vault.example/v2","authSecret":"Bearer v2"}]}}`); s != http.StatusOK || !strings.Contains(resp, `"authSecretSet":true`) {
		t.Fatalf("changed url with secret: %d %s", s, resp)
	}
	if moved, _ := secret(); moved == sealed {
		t.Fatal("the new secret was not stored")
	}
	if s, resp := patch(`{"managed":{"provider":"openrouter","model":"m2","mcpServers":[{"name":"vault","url":"https://vault.example/v2","authSecret":""}]}}`); s != http.StatusOK || strings.Contains(resp, "authSecretSet") {
		t.Fatalf("clearing the secret: %d %s", s, resp)
	}
	if _, has := secret(); has {
		t.Fatal("an empty secret did not clear the stored one")
	}

	noBox, auth2, _, _ := startManagedServer(t, nil)
	if status, resp := doPost(t, noBox.URL+"/v1/agents", auth2, body); status != http.StatusUnprocessableEntity {
		t.Fatalf("no master key: %d %s, want 422", status, resp)
	}
}

// TestManagedAgent_MCPServersAreCheckedOnSave: a server address notify must
// not call, or a name that would make tool names ambiguous, is a 422.
func TestManagedAgent_MCPServersAreCheckedOnSave(t *testing.T) {
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	srv, auth, _, _ := startManagedServer(t, box)
	for id, servers := range map[string]string{
		"bad-url":    `[{"name":"v","url":"::not a url"}]`,
		"plain-http": `[{"name":"v","url":"http://vault.example/mcp"}]`,
		"private-ip": `[{"name":"v","url":"https://192.168.1.10/mcp"}]`,
		"duplicate":  `[{"name":"v","url":"https://a.example"},{"name":"v","url":"https://b.example"}]`,
		"bad-name":   `[{"name":"my__vault","url":"https://a.example"}]`,
		"snake-case": `null,"mcp_servers":[{"name":"v","url":"https://a.example"}]`,
	} {
		s, resp := doPost(t, srv.URL+"/v1/agents", auth, `{"identifier":"`+id+`","runtime":"managed","managed":{"provider":"groq","model":"m","mcpServers":`+servers+`}}`)
		if s != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", id, s, resp)
		}
	}
	loopback := `{"identifier":"%s","runtime":"managed","managed":{"provider":"groq","model":"m","mcpServers":[{"name":"v","url":"http://localhost:9000/mcp"}]}}`
	if s, resp := doPost(t, srv.URL+"/v1/agents", auth, fmt.Sprintf(loopback, "prod-loopback")); s != http.StatusUnprocessableEntity {
		t.Errorf("loopback http in production: %d %s, want 422", s, resp)
	}
	dev, devAuth, _, _ := startManagedServer(t, box, func(s *agentsapi.Service) *agentsapi.Service { return s.WithLoopbackMCP(true) })
	if s, resp := doPost(t, dev.URL+"/v1/agents", devAuth, fmt.Sprintf(loopback, "dev-loopback")); s != http.StatusCreated {
		t.Errorf("loopback http in development: %d %s, want 201", s, resp)
	}
}

// TestManagedAgent_RejectedConfigNeverEchoesTheSecret: a refused config's
// 422 body and the server's logs never carry an auth secret.
func TestManagedAgent_RejectedConfigNeverEchoesTheSecret(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	srv, auth, _, _ := startManagedServer(t, box)
	const secret = "Bearer SECRET-MARKER"
	for id, server := range map[string]string{
		"bad-url":    `{"name":"v","url":"https://SECRET-MARKER@[::1","authSecret":"` + secret + `"}`,
		"bad-header": `{"name":"v","url":"https://a.example","authHeader":"Host","authSecret":"` + secret + `"}`,
		"bad-name":   `{"name":"SECRET-MARKER","url":"https://a.example","authSecret":"` + secret + `"}`,
		"bad-type":   `{"name":"v","url":"https://a.example","authSecret":["` + secret + `"]}`,
		"private":    `{"name":"v","url":"https://10.0.0.1/","authSecret":"` + secret + `"}`,
	} {
		s, resp := doPost(t, srv.URL+"/v1/agents", auth, `{"identifier":"`+id+`","runtime":"managed","managed":{"provider":"groq","model":"m","mcpServers":[`+server+`]}}`)
		if s != http.StatusUnprocessableEntity || strings.Contains(resp, "SECRET-MARKER") {
			t.Errorf("%s: %d %s", id, s, resp)
		}
	}
	if strings.Contains(logs.String(), "SECRET-MARKER") {
		t.Fatalf("logs carry the secret:\n%s", logs.String())
	}
}

// TestManagedAgent_KeyIsNotCarriedToANewEndpoint: a patch that moves the
// model endpoint must send the API key again.
func TestManagedAgent_KeyIsNotCarriedToANewEndpoint(t *testing.T) {
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	srv, auth, _, _ := startManagedServer(t, box)
	if s, resp := doPost(t, srv.URL+"/v1/agents", auth, `{"identifier":"keyed","runtime":"managed","managed":{"provider":"my-vllm","baseUrl":"https://llm.example/v1","model":"m","apiKey":"sk-secret"}}`); s != http.StatusCreated {
		t.Fatalf("create: %d %s", s, resp)
	}
	patch := func(b string) (int, string) {
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/keyed", strings.NewReader(b))
		req.Header.Set("Authorization", auth)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		return r.StatusCode, buf.String()
	}
	if s, resp := patch(`{"managed":{"provider":"my-vllm","baseUrl":"https://evil.example/v1","model":"m"}}`); s != http.StatusUnprocessableEntity || !strings.Contains(resp, "apiKey") {
		t.Fatalf("moved baseUrl without key: %d %s, want 422", s, resp)
	}
	if s, resp := patch(`{"managed":{"provider":"my-vllm","baseUrl":"https://llm.example/v1","model":"m2"}}`); s != http.StatusOK || !strings.Contains(resp, `"apiKeySet":true`) {
		t.Fatalf("same baseUrl without key: %d %s", s, resp)
	}
	if s, resp := patch(`{"managed":{"provider":"my-vllm","baseUrl":"https://other.example/v1","model":"m","apiKey":"sk-other"}}`); s != http.StatusOK || !strings.Contains(resp, `"apiKeySet":true`) {
		t.Fatalf("moved baseUrl with key: %d %s", s, resp)
	}
}
