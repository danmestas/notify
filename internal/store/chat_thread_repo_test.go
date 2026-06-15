package store

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestChatThreadRepo_UpsertGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	in := ChatThread{
		AgentIdentifier:  "support",
		PlatformThreadID: "conv-1",
		SubscriberID:     "tg:42",
		Provider:         "telegram",
		ChatID:           "987654321",
	}
	got, err := s.ChatThreads().Upsert(ctx, scope, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got.ID == "" {
		t.Fatalf("Upsert: empty id")
	}
	if got.AgentIdentifier != "support" || got.PlatformThreadID != "conv-1" ||
		got.SubscriberID != "tg:42" || got.Provider != "telegram" || got.ChatID != "987654321" {
		t.Fatalf("Upsert round-trip mismatch: %+v", got)
	}

	byChat, err := s.ChatThreads().GetByChat(ctx, scope, "telegram", "987654321")
	if err != nil {
		t.Fatalf("GetByChat: %v", err)
	}
	if byChat.ID != got.ID || byChat.AgentIdentifier != "support" {
		t.Fatalf("GetByChat mismatch: %+v", byChat)
	}

	byThread, err := s.ChatThreads().GetByThread(ctx, scope, "conv-1")
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	if byThread.ID != got.ID || byThread.ChatID != "987654321" {
		t.Fatalf("GetByThread mismatch: %+v", byThread)
	}
}

func TestChatThreadRepo_UpsertConflictUpdatesNotDuplicates(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	first, err := s.ChatThreads().Upsert(ctx, scope, ChatThread{
		AgentIdentifier:  "support",
		PlatformThreadID: "conv-1",
		SubscriberID:     "tg:42",
		Provider:         "telegram",
		ChatID:           "chat-A",
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Re-upsert the same (provider, chat_id) with a refreshed thread+agent: the
	// row id must be stable and the mutable fields must reflect the new values.
	second, err := s.ChatThreads().Upsert(ctx, scope, ChatThread{
		AgentIdentifier:  "billing",
		PlatformThreadID: "conv-1",
		SubscriberID:     "tg:42",
		Provider:         "telegram",
		ChatID:           "chat-A",
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("Upsert minted a new row on conflict: %s -> %s", first.ID, second.ID)
	}
	if second.AgentIdentifier != "billing" {
		t.Fatalf("Upsert did not refresh agent_identifier: %+v", second)
	}

	// Exactly one row for the key.
	var n int
	if err := s.reader.QueryRow(
		`SELECT COUNT(*) FROM chat_threads WHERE org_id=? AND environment_id=? AND provider=? AND chat_id=?`,
		scope.OrgID, scope.EnvID, "telegram", "chat-A",
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row after re-upsert, got %d", n)
	}
}

func TestChatThreadRepo_TenantIsolation(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scopeA := domain.Scope{OrgID: "o1", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "o2", EnvID: "e2"}

	if _, err := s.ChatThreads().Upsert(ctx, scopeA, ChatThread{
		AgentIdentifier:  "support",
		PlatformThreadID: "conv-A",
		SubscriberID:     "tg:1",
		Provider:         "telegram",
		ChatID:           "shared-chat",
	}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}

	// Same provider+chat_id key, different tenant: must not read A's row.
	if _, err := s.ChatThreads().GetByChat(ctx, scopeB, "telegram", "shared-chat"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByChat across tenant: err = %v, want ErrNotFound", err)
	}
	if _, err := s.ChatThreads().GetByThread(ctx, scopeB, "conv-A"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByThread across tenant: err = %v, want ErrNotFound", err)
	}

	// B can write its own row under the same key without disturbing A.
	if _, err := s.ChatThreads().Upsert(ctx, scopeB, ChatThread{
		AgentIdentifier:  "other",
		PlatformThreadID: "conv-B",
		SubscriberID:     "tg:2",
		Provider:         "telegram",
		ChatID:           "shared-chat",
	}); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}
	a, err := s.ChatThreads().GetByChat(ctx, scopeA, "telegram", "shared-chat")
	if err != nil {
		t.Fatalf("GetByChat A after B write: %v", err)
	}
	if a.AgentIdentifier != "support" || a.PlatformThreadID != "conv-A" {
		t.Fatalf("tenant A row mutated by tenant B: %+v", a)
	}
}
