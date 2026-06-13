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

// TestConversationRepo_ListByAgent proves ListByAgent: ordering by
// last_activity_at DESC, keyset pagination, and cross-env isolation.
func TestConversationRepo_ListByAgent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Conversations()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	const agentID = "ag-list"

	// Create 5 conversations in scope; a 6th in a different scope (must not appear).
	for i := 0; i < 5; i++ {
		if _, err := repo.Upsert(ctx, scope, agentdomain.Conversation{
			AgentID:          agentID,
			PlatformThreadID: fmt.Sprintf("thread-%d", i),
			SubscriberID:     fmt.Sprintf("sub-%d", i),
		}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	// Cross-env conversation — must not appear in scope's list.
	if _, err := repo.Upsert(ctx, other, agentdomain.Conversation{
		AgentID:          agentID,
		PlatformThreadID: "thread-other",
	}); err != nil {
		t.Fatalf("upsert other: %v", err)
	}

	// Default first page (limit=0 → 50): all 5 in scope, sorted DESC.
	page, err := repo.ListByAgent(ctx, scope, agentID, 0, "")
	if err != nil {
		t.Fatalf("ListByAgent: %v", err)
	}
	if len(page) != 5 {
		t.Fatalf("want 5 conversations, got %d", len(page))
	}
	// Verify ordering: last_activity_at DESC (thread-4 is most recent).
	for i, c := range page[:len(page)-1] {
		if c.LastActivityAt < page[i+1].LastActivityAt {
			t.Errorf("order violation at [%d]: %s < %s", i, c.LastActivityAt, page[i+1].LastActivityAt)
		}
	}

	// Cross-env list must be empty.
	otherPage, err := repo.ListByAgent(ctx, other, agentID, 0, "")
	if err != nil {
		t.Fatalf("ListByAgent other: %v", err)
	}
	if len(otherPage) != 1 {
		t.Fatalf("other scope: want 1 conversation, got %d", len(otherPage))
	}

	// Keyset pagination: limit=2 fetches first two; passing last id fetches next two.
	p1, err := repo.ListByAgent(ctx, scope, agentID, 2, "")
	if err != nil {
		t.Fatalf("ListByAgent page 1: %v", err)
	}
	if len(p1) != 2 {
		t.Fatalf("page 1: want 2, got %d", len(p1))
	}

	p2, err := repo.ListByAgent(ctx, scope, agentID, 2, p1[len(p1)-1].ID)
	if err != nil {
		t.Fatalf("ListByAgent page 2: %v", err)
	}
	if len(p2) != 2 {
		t.Fatalf("page 2: want 2, got %d", len(p2))
	}

	p3, err := repo.ListByAgent(ctx, scope, agentID, 2, p2[len(p2)-1].ID)
	if err != nil {
		t.Fatalf("ListByAgent page 3: %v", err)
	}
	if len(p3) != 1 {
		t.Fatalf("page 3 (last): want 1, got %d", len(p3))
	}

	// All three pages together must equal the full result with no duplicates.
	allIDs := map[string]bool{}
	for _, c := range append(append(p1, p2...), p3...) {
		if allIDs[c.ID] {
			t.Errorf("duplicate id %s across pages", c.ID)
		}
		allIDs[c.ID] = true
	}
	if len(allIDs) != 5 {
		t.Fatalf("union of pages: want 5 distinct ids, got %d", len(allIDs))
	}

	// Empty agent returns empty slice (not nil/error).
	empty, err := repo.ListByAgent(ctx, scope, "nonexistent-agent", 0, "")
	if err != nil {
		t.Fatalf("ListByAgent for unknown agent: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown agent: want 0, got %d", len(empty))
	}
}

// TestActivityRepo_Page proves ActivityRepo.Page: before, after, no-cursor, and
// cross-env isolation. The test uses a full-transcript call to establish the
// canonical ordered slice, then validates cursor results against that ordering.
func TestActivityRepo_Page(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Activities()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o1", EnvID: "e2"}

	const convID = "conv-page"

	// Insert 6 activities into scope.
	for i := 0; i < 6; i++ {
		if _, err := repo.Append(ctx, scope, agentdomain.Activity{
			ConversationID: convID,
			Role:           "user",
			Content:        json.RawMessage(fmt.Sprintf(`{"parts":[{"text":"msg%d"}]}`, i)),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Cross-env activity — must not appear in scope's pages.
	if _, err := repo.Append(ctx, other, agentdomain.Activity{
		ConversationID: convID,
		Role:           "agent",
		Content:        json.RawMessage(`{"parts":[{"text":"xenv"}]}`),
	}); err != nil {
		t.Fatalf("append other: %v", err)
	}

	// Establish canonical order via the existing History (full transcript).
	all6, err := repo.History(ctx, scope, convID, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(all6) != 6 {
		t.Fatalf("History: want 6, got %d", len(all6))
	}
	// ids[0..5] in oldest-first order (History guarantees this).
	canonicalIDs := make([]string, len(all6))
	for i, a := range all6 {
		canonicalIDs[i] = a.ID
	}

	// No cursor → most recent 3 oldest-first (canonical[3..5]).
	noCursor, err := repo.Page(ctx, scope, convID, 3, "", "")
	if err != nil {
		t.Fatalf("Page no-cursor: %v", err)
	}
	if len(noCursor) != 3 {
		t.Fatalf("no-cursor: want 3, got %d", len(noCursor))
	}
	for i, want := range canonicalIDs[3:] {
		if noCursor[i].ID != want {
			t.Errorf("no-cursor[%d] id=%s, want %s", i, noCursor[i].ID, want)
		}
	}

	// before=canonical[4] → rows older than canonical[4], newest-of-old first
	// limited to 3, then oldest-first → canonical[1..3].
	beforePage, err := repo.Page(ctx, scope, convID, 3, canonicalIDs[4], "")
	if err != nil {
		t.Fatalf("Page before: %v", err)
	}
	if len(beforePage) != 3 {
		t.Fatalf("before: want 3, got %d", len(beforePage))
	}
	for i, want := range canonicalIDs[1:4] {
		if beforePage[i].ID != want {
			t.Errorf("before[%d] id=%s, want %s", i, beforePage[i].ID, want)
		}
	}

	// after=canonical[2] → rows newer than canonical[2]: canonical[3..5].
	afterPage, err := repo.Page(ctx, scope, convID, 10, "", canonicalIDs[2])
	if err != nil {
		t.Fatalf("Page after: %v", err)
	}
	if len(afterPage) != 3 {
		t.Fatalf("after: want 3, got %d", len(afterPage))
	}
	for i, want := range canonicalIDs[3:] {
		if afterPage[i].ID != want {
			t.Errorf("after[%d] id=%s, want %s", i, afterPage[i].ID, want)
		}
	}

	// Cross-env isolation: other scope sees only its own activity.
	otherPage, err := repo.Page(ctx, other, convID, 0, "", "")
	if err != nil {
		t.Fatalf("Page other scope: %v", err)
	}
	if len(otherPage) != 1 {
		t.Fatalf("other scope: want 1, got %d", len(otherPage))
	}

	// Limit cap: limit=0 → default (50), must return all 6.
	allPage, err := repo.Page(ctx, scope, convID, 0, "", "")
	if err != nil {
		t.Fatalf("Page limit=0: %v", err)
	}
	if len(allPage) != 6 {
		t.Fatalf("limit=0: want 6, got %d", len(allPage))
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
