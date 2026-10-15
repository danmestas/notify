package wire_test

import (
	"encoding/json"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/wire"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestFormat_Golden pins the exact JSON of each type: names, order, which
// fields are omitted, and that tenancy never appears.
func TestFormat_Golden(t *testing.T) {
	cases := []struct {
		name string
		got  any
		want string
	}{
		{
			"agent",
			wire.FromAgent(agentdomain.Agent{
				ID: "a1", OrgID: "org", EnvID: "env", Identifier: "support", Name: "Support",
				Runtime: "mesh", Active: true, MeshSubject: "agents.support", CreatedAt: "t0", UpdatedAt: "t1",
			}),
			`{"id":"a1","identifier":"support","name":"Support","description":"","runtime":"mesh","active":true,"meshSubject":"agents.support","bridgeUrl":"","welcomeMessage":"","createdAt":"t0","updatedAt":"t1"}`,
		},
		{
			"link",
			wire.FromLink(agentdomain.AgentLink{ID: "l1", OrgID: "org", EnvID: "env", AgentID: "a1", IntegrationID: "i1", ConnectedAt: "t0", Config: json.RawMessage(`{"chatId":"7"}`)}),
			`{"id":"l1","agentId":"a1","integrationId":"i1","connectedAt":"t0","config":{"chatId":"7"}}`,
		},
		{
			"conversation",
			wire.FromConversation(agentdomain.Conversation{
				ID: "c1", OrgID: "org", EnvID: "env", AgentID: "a1", PlatformThreadID: "th", SubscriberID: "s1",
				Status: "open", CreatedAt: "t0", LastActivityAt: "t1",
			}),
			`{"id":"c1","agentId":"a1","integrationId":"","platformThreadId":"th","subscriberId":"s1","status":"open","createdAt":"t0","lastActivityAt":"t1"}`,
		},
		{
			"agent activity with an action",
			wire.FromActivity(agentdomain.Activity{
				ID: "x1", OrgID: "org", EnvID: "env", ConversationID: "c1", Role: "agent", RunID: "r1", CreatedAt: "t0",
				Content: json.RawMessage(`{"parts":[{"text":"hi"}],"action":{"label":"Yes","value":"yes"}}`),
			}),
			`{"id":"x1","conversationId":"c1","role":"agent","content":{"parts":[{"type":"text","text":"hi"}],"action":{"label":"Yes","value":"yes"}},"runId":"r1","createdAt":"t0"}`,
		},
		{
			"user activity",
			wire.FromActivity(agentdomain.Activity{
				ID: "x2", ConversationID: "c1", Role: "user", CreatedAt: "t0", ClientMessageID: "k1",
				Content: json.RawMessage(`{"parts":[{"text":"a \"quote\""}]}`),
			}),
			`{"id":"x2","conversationId":"c1","role":"user","content":{"parts":[{"type":"text","text":"a \"quote\""}]},"createdAt":"t0"}`,
		},
		{
			"empty lists",
			[]any{wire.Agents(nil), wire.Conversations(nil), wire.Activities(nil)},
			`[[],[],[]]`,
		},
	}
	for _, c := range cases {
		if got := mustJSON(t, c.got); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// TestContent_UnreadableBodyIsEmpty: a stored body that is not a message
// body becomes empty parts, never an error.
func TestContent_UnreadableBodyIsEmpty(t *testing.T) {
	for _, raw := range []string{``, `null`, `"plain string"`, `[1,2]`, `{"parts":"nope"}`, `{"other":1}`} {
		got := mustJSON(t, wire.FromActivity(agentdomain.Activity{ID: "x", Content: json.RawMessage(raw)}).Content)
		if got != `{"parts":[]}` {
			t.Errorf("content %q -> %s, want empty parts", raw, got)
		}
	}
}

// TestContent_KeepsAStoredPartType: a part already carrying a type keeps it.
func TestContent_KeepsAStoredPartType(t *testing.T) {
	c := wire.FromActivity(agentdomain.Activity{Content: json.RawMessage(`{"parts":[{"type":"image","text":""}]}`)}).Content
	if len(c.Parts) != 1 || c.Parts[0].Type != "image" {
		t.Fatalf("parts = %+v, want one image part", c.Parts)
	}
}
