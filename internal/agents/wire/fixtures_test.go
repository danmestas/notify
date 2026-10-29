package wire_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/agents/wire"
)

var update = flag.Bool("update", false, "rewrite testdata fixtures from the current wire types")

// fixtures is one fully populated value per wire type. testdata/<name>.json is
// the contract a client pins: E copies these files and tests its reader
// against them, so a field notify renames or removes fails here first.
var fixtures = []struct {
	name string
	typ  reflect.Type
	v    any
}{
	{"agent", reflect.TypeOf(wire.Agent{}), wire.FromAgent(agentdomain.Agent{
		ID: "a1", Identifier: "support", Name: "Support", Description: "Answers support mail",
		Runtime: "managed", Active: true, MeshSubject: "agents.support", BridgeURL: "https://bridge.example",
		Managed: json.RawMessage(`{"integrationId":"i1"}`), WelcomeMessage: "Hi", Labels: map[string]string{"owner": "lou"}, Logo: "data:image/png;base64,iVBORw0KGgo=", CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-02T00:00:00Z",
	})},
	{"agent_managed", reflect.TypeOf(wire.Agent{}), wire.FromAgent(agentdomain.Agent{
		ID: "a2", Identifier: "ember", Name: "Ember", Runtime: "managed", Active: true,
		Managed:   json.RawMessage(`{"provider":"openrouter","model":"anthropic/claude-sonnet-4.5","systemPrompt":"Draft replies.","apiKey":"enc:1:sealed"}`),
		Labels:    map[string]string{"owner": "owner"},
		CreatedAt: "2026-10-18T00:00:00Z", UpdatedAt: "2026-10-18T00:00:00Z",
	})},
	{"agent_managed_mcp", reflect.TypeOf(wire.Agent{}), wire.FromAgent(agentdomain.Agent{
		ID: "a3", Identifier: "vault-agent", Name: "Vault", Runtime: "managed", Active: true,
		Managed:   json.RawMessage(`{"provider":"openrouter","model":"anthropic/claude-sonnet-4.5","apiKey":"enc:1:sealed","mcpServers":[{"name":"vault","url":"https://relay.example/mcp","authHeader":"Authorization","authSecret":"enc:1:sealed"}]}`),
		Labels:    map[string]string{"owner": "owner"},
		CreatedAt: "2026-10-21T00:00:00Z", UpdatedAt: "2026-10-21T00:00:00Z",
	})},
	{"link", reflect.TypeOf(wire.Link{}), wire.FromLink(agentdomain.AgentLink{
		ID: "l1", AgentID: "a1", IntegrationID: "i1", ConnectedAt: "2026-10-01T00:00:00Z", Config: json.RawMessage(`{"chatId":"7"}`),
	})},
	{"conversation", reflect.TypeOf(wire.Conversation{}), wire.FromConversation(agentdomain.Conversation{
		ID: "c1", AgentID: "a1", IntegrationID: "i1", PlatformThreadID: "th-1", SubscriberID: "s1",
		Status: "open", CreatedAt: "2026-10-01T00:00:00Z", LastActivityAt: "2026-10-02T00:00:00Z",
	})},
	{"conversation_summary", reflect.TypeOf(wire.ConversationSummary{}), wire.FromConversationSummary(agentstore.ConversationSummary{
		ConversationID: "c1", AgentIdentifier: "support", AgentName: "Support", SubscriberID: "s1",
		PlatformThreadID: "th-1", Status: "open", LastMessageAt: "2026-10-02T00:00:00Z", LastActivityID: "x1",
	})},
	{"draft", reflect.TypeOf(wire.Draft{}), sampleDraft()},
	{"draft_list", reflect.TypeOf(wire.DraftList{}), wire.DraftList{
		Drafts: []wire.Draft{sampleDraft()}, Cursor: "42.1792280000000",
	}},
	{"draft_list_gap", reflect.TypeOf(wire.DraftList{}), wire.DraftList{
		Drafts: []wire.Draft{sampleDraft()}, Cursor: "42.1792280000000", Gap: true,
	}},
	{"activity", reflect.TypeOf(wire.Activity{}), wire.FromActivity(agentdomain.Activity{
		ID: "x1", ConversationID: "c1", Role: "agent", RunID: "r1", CreatedAt: "2026-10-02T00:00:00Z",
		Content: json.RawMessage(`{"parts":[{"text":"Shall I reply?"}],"action":{"label":"Yes","value":"yes"},"attachments":[{"name":"invoice.md","mediaType":"text/markdown","text":"# Invoice"}],"sources":[{"server":"vault","tool":"get_record","label":"rec_1","ref":"rec_1","error":true}]}`),
	})},
}

// TestFixtures_MatchTheWireTypes: each wire type marshals exactly to its
// fixture, and each fixture names every field of its type, nested ones
// included. Renaming, removing or adding a field without updating the fixture
// fails, which is the point: a change a client can see is a change to these
// files. Run `go test ./internal/agents/wire -update` after an intended change.
func TestFixtures_MatchTheWireTypes(t *testing.T) {
	for _, f := range fixtures {
		got, err := json.MarshalIndent(f.v, "", "  ")
		if err != nil {
			t.Fatalf("%s: marshal: %v", f.name, err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", f.name+".json")
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatalf("%s: write: %v", f.name, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read fixture: %v (run with -update to create it)", f.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the wire format no longer matches %s\n got  %s\n want %s", f.name, path, got, want)
		}
		var doc map[string]any
		if err := json.Unmarshal(want, &doc); err != nil {
			t.Fatalf("%s: fixture is not a JSON object: %v", f.name, err)
		}
		checkFields(t, f.name, f.typ, doc)
	}
}

// checkFields fails when a fixture object's keys differ from the JSON fields
// of typ, recursing into struct-typed and slice-of-struct fields.
func checkFields(t *testing.T, where string, typ reflect.Type, doc map[string]any) {
	t.Helper()
	fields := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = sf.Type
	}
	if got, want := sortedKeys(doc), sortedKeys(fields); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: fixture fields %v, type fields %v: populate every field so a client sees it", where, got, want)
		return
	}
	for name, ft := range fields {
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		switch v := doc[name].(type) {
		case map[string]any:
			checkFields(t, where+"."+name, ft, v)
		case []any:
			for _, e := range v {
				if m, ok := e.(map[string]any); ok {
					checkFields(t, where+"."+name+"[]", ft, m)
				}
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sampleDraft() wire.Draft {
	return wire.FromDraft(agentdomain.Draft{
		Seq: 42, ID: "01JDRAFT00000000000000000A", SubscriberID: "s1", Mailbox: "acct-1",
		AgentIdentifier: "support", AgentName: "Support", ConversationID: "th-1", ActivityID: "x1",
		SourceMessageID: "msg-7", InReplyTo: "<orig@example.com>", References: []string{"<root@example.com>", "<orig@example.com>"},
		Subject: "Re: Invoice", To: []string{"ann@example.com"}, Text: "Thanks, **paid** today.", CreatedAt: "2026-10-17T12:00:00.000Z",
	})
}
