package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// openTemp opens the agents Store over a fresh temp-dir core store,
// registering cleanup.
func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := corestore.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st)
}

func TestAgentRepo_CRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Agents()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := repo.Create(ctx, scope, agentdomain.Agent{
		Identifier:  "support-bot",
		Name:        "Support Bot",
		Description: "answers tickets",
		Runtime:     agentdomain.RuntimeMesh,
		Active:      true,
		MeshSubject: "agents.support.prompt",
		Managed:     json.RawMessage(`{"system_prompt":"be kind"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" || created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("create must mint id and timestamps: %+v", created)
	}

	got, err := repo.GetByIdentifier(ctx, scope, "support-bot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID || got.Runtime != "mesh" || got.MeshSubject != "agents.support.prompt" || !got.Active {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if string(got.Managed) != `{"system_prompt":"be kind"}` {
		t.Fatalf("managed JSON round-trip mismatch: %s", got.Managed)
	}

	got.Name = "Support Bot v2"
	got.Runtime = agentdomain.RuntimeBridge
	got.BridgeURL = "https://bot.example.com/turn"
	got.Active = false
	updated, err := repo.Update(ctx, scope, got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Support Bot v2" || updated.Runtime != "bridge" || updated.BridgeURL != "https://bot.example.com/turn" || updated.Active {
		t.Fatalf("update not applied: %+v", updated)
	}
	if updated.ID != created.ID {
		t.Fatalf("update changed id: %s -> %s", created.ID, updated.ID)
	}

	list, err := repo.List(ctx, scope)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Identifier != "support-bot" {
		t.Fatalf("expected [support-bot], got %+v", list)
	}

	// Update/Delete on an unknown identifier -> ErrNotFound.
	if _, err := repo.Update(ctx, scope, agentdomain.Agent{Identifier: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for ghost update, got %v", err)
	}
	if err := repo.Delete(ctx, scope, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for ghost delete, got %v", err)
	}

	if err := repo.Delete(ctx, scope, "support-bot"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByIdentifier(ctx, scope, "support-bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestAgentRepo_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Agents()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	if _, err := repo.Create(ctx, scope, agentdomain.Agent{Identifier: "a", Runtime: "mesh"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := repo.GetByIdentifier(ctx, other, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound across envs, got %v", err)
	}
	list, err := repo.List(ctx, other)
	if err != nil {
		t.Fatalf("list e2: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("e2 must not see e1 agents, got %+v", list)
	}
	if err := repo.Delete(ctx, other, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env delete must not match, got %v", err)
	}
}

func TestAgentLinkRepo_LinkListUnlink(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Links()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	link, err := repo.Link(ctx, scope, agentdomain.AgentLink{
		AgentID:       "ag-1",
		IntegrationID: "int-1",
		Config:        json.RawMessage(`{"channel":"#support"}`),
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if link.ID == "" || link.ConnectedAt == "" {
		t.Fatalf("link must mint id and connected_at: %+v", link)
	}

	// Re-linking the same integration updates config and keeps the id.
	relink, err := repo.Link(ctx, scope, agentdomain.AgentLink{
		AgentID:       "ag-1",
		IntegrationID: "int-1",
		Config:        json.RawMessage(`{"channel":"#general"}`),
	})
	if err != nil {
		t.Fatalf("relink: %v", err)
	}
	if relink.ID != link.ID {
		t.Fatalf("relink changed id: %s -> %s", link.ID, relink.ID)
	}
	if string(relink.Config) != `{"channel":"#general"}` {
		t.Fatalf("relink config not updated: %s", relink.Config)
	}

	links, err := repo.ListByAgent(ctx, scope, "ag-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(links) != 1 || links[0].IntegrationID != "int-1" {
		t.Fatalf("expected [int-1], got %+v", links)
	}

	// Cross-env isolation: e2 sees no links and cannot unlink.
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}
	otherLinks, err := repo.ListByAgent(ctx, other, "ag-1")
	if err != nil {
		t.Fatalf("list e2: %v", err)
	}
	if len(otherLinks) != 0 {
		t.Fatalf("e2 must not see e1 links, got %+v", otherLinks)
	}
	if err := repo.Unlink(ctx, other, "ag-1", "int-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env unlink must not match, got %v", err)
	}

	if err := repo.Unlink(ctx, scope, "ag-1", "int-1"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if err := repo.Unlink(ctx, scope, "ag-1", "int-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for second unlink, got %v", err)
	}
}

func TestConversationRepo_UpsertDedupes(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Conversations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	conv, err := repo.Upsert(ctx, scope, agentdomain.Conversation{
		AgentID:          "ag-1",
		PlatformThreadID: "thread-1",
		SubscriberID:     "sub-1",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if conv.ID == "" || conv.Status != "open" || conv.CreatedAt == "" || conv.LastActivityAt == "" {
		t.Fatalf("unexpected conversation: %+v", conv)
	}

	// Re-upserting the same thread keeps the row and refreshes last_activity_at.
	again, err := repo.Upsert(ctx, scope, agentdomain.Conversation{
		AgentID:          "ag-1",
		PlatformThreadID: "thread-1",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if again.ID != conv.ID {
		t.Fatalf("re-upsert changed id: %s -> %s", conv.ID, again.ID)
	}
	if again.LastActivityAt < conv.LastActivityAt {
		t.Fatalf("last_activity_at went backwards: %s -> %s", conv.LastActivityAt, again.LastActivityAt)
	}

	got, err := repo.Get(ctx, scope, conv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SubscriberID != "sub-1" || got.PlatformThreadID != "thread-1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Cross-env isolation.
	if _, err := repo.Get(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, conv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound across envs, got %v", err)
	}
}

func TestActivityRepo_AppendAndHistoryWindow(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Activities()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	for i := 0; i < 5; i++ {
		role := "user"
		if i%2 == 1 {
			role = "agent"
		}
		content := json.RawMessage(fmt.Sprintf(`{"parts":[{"text":"msg %d"}]}`, i))
		if _, err := repo.Append(ctx, scope, agentdomain.Activity{
			ConversationID: "conv-1",
			Role:           role,
			Content:        content,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// History(n=3) is the last 3 activities oldest-first.
	hist, err := repo.History(ctx, scope, "conv-1", 3)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("expected 3 activities, got %d", len(hist))
	}
	for i, want := range []string{`{"parts":[{"text":"msg 2"}]}`, `{"parts":[{"text":"msg 3"}]}`, `{"parts":[{"text":"msg 4"}]}`} {
		if string(hist[i].Content) != want {
			t.Fatalf("history[%d] = %s, want %s", i, hist[i].Content, want)
		}
	}

	// n <= 0 returns the full transcript.
	full, err := repo.History(ctx, scope, "conv-1", 0)
	if err != nil {
		t.Fatalf("full history: %v", err)
	}
	if len(full) != 5 {
		t.Fatalf("expected 5 activities, got %d", len(full))
	}

	// Cross-env isolation.
	otherHist, err := repo.History(ctx, domain.Scope{OrgID: "o1", EnvID: "e2"}, "conv-1", 0)
	if err != nil {
		t.Fatalf("history e2: %v", err)
	}
	if len(otherHist) != 0 {
		t.Fatalf("e2 must not see e1 activities, got %+v", otherHist)
	}
}

func TestAgentLinkRepo_UpsertScopeMismatch(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Links()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	link, err := repo.Link(ctx, scope, agentdomain.AgentLink{
		AgentID:       "ag-1",
		IntegrationID: "int-1",
		Config:        json.RawMessage(`{"channel":"#support"}`),
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}

	// An upsert from env B colliding with env A's (agent_id, integration_id)
	// row must not mutate it: the scope-guarded DO UPDATE skips, and B's
	// scoped read-back surfaces ErrNotFound.
	if _, err := repo.Link(ctx, other, agentdomain.AgentLink{
		AgentID:       "ag-1",
		IntegrationID: "int-1",
		Config:        json.RawMessage(`{"channel":"#hijacked"}`),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-env upsert, got %v", err)
	}

	got, err := repo.ListByAgent(ctx, scope, "ag-1")
	if err != nil {
		t.Fatalf("list after cross-env upsert: %v", err)
	}
	if len(got) != 1 || got[0].ID != link.ID || string(got[0].Config) != `{"channel":"#support"}` {
		t.Fatalf("cross-env upsert mutated e1's link: %+v", got)
	}
}

func TestConversationRepo_UpsertScopeMismatch(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Conversations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	conv, err := repo.Upsert(ctx, scope, agentdomain.Conversation{
		AgentID:          "ag-1",
		PlatformThreadID: "thread-1",
		SubscriberID:     "sub-1",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// An upsert from env B colliding with env A's dedup key must not mutate
	// A's row: the scope-guarded DO UPDATE skips, and B's scoped read-back
	// surfaces ErrNotFound.
	if _, err := repo.Upsert(ctx, other, agentdomain.Conversation{
		AgentID:          "ag-1",
		PlatformThreadID: "thread-1",
		SubscriberID:     "sub-evil",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-env upsert, got %v", err)
	}

	got, err := repo.Get(ctx, scope, conv.ID)
	if err != nil {
		t.Fatalf("get after cross-env upsert: %v", err)
	}
	if got.SubscriberID != "sub-1" || got.LastActivityAt != conv.LastActivityAt {
		t.Fatalf("cross-env upsert mutated e1's conversation: %+v (want %+v)", got, conv)
	}
}

// TestConversationRepo_GetByThread proves the read-side counterpart of
// Upsert: the dedup key resolves the row without creating one, scoped, with
// ErrNotFound for absent threads.
func TestConversationRepo_GetByThread(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := s.Conversations().Upsert(ctx, scope, agentdomain.Conversation{
		AgentID:          "ag1",
		IntegrationID:    "",
		PlatformThreadID: "thread-1",
		SubscriberID:     "sub1",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.Conversations().GetByThread(ctx, scope, "ag1", "", "thread-1")
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("id = %q, want %q", got.ID, created.ID)
	}

	if _, err := s.Conversations().GetByThread(ctx, scope, "ag1", "", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing thread err = %v, want ErrNotFound", err)
	}
	// Scope isolation: another tenant must not see the row.
	other := domain.Scope{OrgID: "o2", EnvID: "e2"}
	if _, err := s.Conversations().GetByThread(ctx, other, "ag1", "", "thread-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}
