package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
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

func startManagedServer(t *testing.T, box *secretbox.Box) (*httptest.Server, string, *agentstore.Store, domain.Scope) {
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
