package ingress_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
)

// The agent API's object key sets (#9). A rename in either direction fails
// here before it reaches a client.
var (
	agentKeys        = []string{"active", "bridgeUrl", "createdAt", "description", "id", "identifier", "labels", "meshSubject", "name", "runtime", "updatedAt", "welcomeMessage"}
	linkKeys         = []string{"agentId", "config", "connectedAt", "id", "integrationId"}
	conversationKeys = []string{"agentId", "createdAt", "id", "integrationId", "lastActivityAt", "platformThreadId", "status", "subscriberId"}
	summaryKeys      = []string{"agentIdentifier", "agentName", "conversationId", "lastActivityId", "lastMessageAt", "platformThreadId", "status", "subscriberId"}
	activityKeys     = []string{"content", "conversationId", "createdAt", "id", "role"}
	agentActKeys     = []string{"content", "conversationId", "createdAt", "id", "role", "runId"}
)

// TestAgentAPI_ResponsesAreCamelCase is issue 9 and 10: every agent API
// response uses the defined camelCase format, carries no tenancy, and a
// message body is typed {parts:[{type,text}]}.
func TestAgentAPI_ResponsesAreCamelCase(t *testing.T) {
	h := newHarness(t)
	h.startStub(t)
	res := h.compose(t, "", "hello there")
	waitAgentReply(t, h, res.ConversationID)

	r := chi.NewRouter()
	agentsapi.New(h.agents).Routes(r)
	api := httptest.NewServer(scopeInjector(testScope, r))
	t.Cleanup(api.Close)
	id := h.agent.Identifier

	get := func(base, path string) any {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, raw)
		}
		assertCamelCase(t, path, v)
		return v
	}

	keysOf(t, "agent", get(api.URL, "/v1/agents/"+id), agentKeys)
	for _, a := range get(api.URL, "/v1/agents").([]any) {
		keysOf(t, "agent list", a, agentKeys)
	}
	convs := get(api.URL, "/v1/agents/"+id+"/conversations").([]any)
	if len(convs) != 1 {
		t.Fatalf("conversations: %d, want 1", len(convs))
	}
	keysOf(t, "conversation list", convs[0], conversationKeys)

	global := get(api.URL, "/v1/inbox/conversations").(map[string]any)
	items := global["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("global conversations: %d, want 1", len(items))
	}
	keysOf(t, "global conversation", items[0], summaryKeys)

	resp, err := http.Post(api.URL+"/v1/agents/"+id+"/integrations", "application/json",
		strings.NewReader(`{"integrationId":"int_1","config":{"chatId":"7"}}`))
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	var link any
	_ = json.NewDecoder(resp.Body).Decode(&link)
	_ = resp.Body.Close()
	assertCamelCase(t, "link", link)
	keysOf(t, "link", link, linkKeys)
	for _, l := range get(api.URL, "/v1/agents/"+id+"/integrations").([]any) {
		keysOf(t, "link list", l, linkKeys)
	}

	tr := get(h.srv.URL, "/v1/inbox/conversations/"+id+"?conversationId="+res.ConversationID).(map[string]any)
	keysOf(t, "transcript conversation", tr["conversation"], conversationKeys)
	acts := tr["activities"].([]any)
	if len(acts) != 2 {
		t.Fatalf("activities: %d, want 2", len(acts))
	}
	keysOf(t, "user activity", acts[0], activityKeys)
	keysOf(t, "agent activity", acts[1], agentActKeys)
	wantContent := map[string]any{"parts": []any{map[string]any{"type": "text", "text": "hello there"}}}
	if got := acts[0].(map[string]any)["content"]; !reflect.DeepEqual(got, wantContent) {
		t.Errorf("user content = %v, want %v", got, wantContent)
	}
	reply := acts[1].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(reply) != 1 || reply[0].(map[string]any)["type"] != "text" || reply[0].(map[string]any)["text"] != "ECHO::hello there" {
		t.Errorf("agent content parts = %v", reply)
	}
}

// waitAgentReply waits for the agent's reply to land in the transcript.
func waitAgentReply(t *testing.T, h *harness, threadID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, raw := h.transcriptOf(t, h.agent.Identifier, threadID)
		if strings.Contains(string(raw), `"role":"agent"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no agent reply within 15s")
}

// assertCamelCase fails on any object key that is not lower camelCase, and on
// any tenancy key, anywhere in v. Object values the caller supplied (an
// agent's managed config, a link's config) are skipped.
func assertCamelCase(t *testing.T, where string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "" || k[0] < 'a' || k[0] > 'z' || strings.ContainsAny(k, "_-") {
				t.Errorf("%s: key %q is not camelCase", where, k)
			}
			switch strings.ToLower(k) {
			case "orgid", "envid", "environmentid":
				t.Errorf("%s: tenancy key %q in response", where, k)
			}
			if k != "config" && k != "managed" {
				assertCamelCase(t, where, child)
			}
		}
	case []any:
		for _, e := range x {
			assertCamelCase(t, where, e)
		}
	}
}

// keysOf fails unless v is an object with exactly the keys want.
func keysOf(t *testing.T, what string, v any, want []string) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: %T, want an object", what, v)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s keys = %v, want %v", what, got, want)
	}
}
