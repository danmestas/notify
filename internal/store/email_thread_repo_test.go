package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func seedThread(t *testing.T, s *Store, scope domain.Scope, platformThreadID, email, agent, dom string) {
	t.Helper()
	_, err := s.EmailThreads().Upsert(context.Background(), scope, EmailThread{
		AgentIdentifier:  agent,
		PlatformThreadID: platformThreadID,
		SubscriberID:     "sub-" + platformThreadID,
		SubscriberEmail:  email,
		InboundDomain:    dom,
	})
	if err != nil {
		t.Fatalf("seed Upsert %s: %v", platformThreadID, err)
	}
}

func TestEmailThreadRepo_List_ScopeIsolation(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scopeA := domain.Scope{OrgID: "orgA", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "orgB", EnvID: "envB"}

	seedThread(t, s, scopeA, "thr-a1", "a1@example.com", "support", "inbound.a.com")
	seedThread(t, s, scopeA, "thr-a2", "a2@example.com", "support", "inbound.a.com")
	seedThread(t, s, scopeB, "thr-b1", "b1@example.com", "support", "inbound.b.com")

	items, _, total, err := s.EmailThreads().List(ctx, scopeA, "", 50)
	if err != nil {
		t.Fatalf("List A: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	for _, it := range items {
		if it.OrgID != "orgA" || it.EnvID != "envA" {
			t.Errorf("leaked row from other scope: %+v", it)
		}
		if it.PlatformThreadID == "thr-b1" {
			t.Error("scopeB row appeared in scopeA list")
		}
	}
}

func TestEmailThreadRepo_List_EmptyScope(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "fresh", EnvID: "env1"}

	items, next, total, err := s.EmailThreads().List(ctx, scope, "", 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if next != "" {
		t.Errorf("next = %q, want empty", next)
	}
}

func TestEmailThreadRepo_List_CursorPagination(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	seedThread(t, s, scope, "thr-1", "1@example.com", "support", "inbound.com")
	seedThread(t, s, scope, "thr-2", "2@example.com", "support", "inbound.com")
	seedThread(t, s, scope, "thr-3", "3@example.com", "support", "inbound.com")

	page1, next, total, err := s.EmailThreads().List(ctx, scope, "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if next == "" {
		t.Fatal("page1 next cursor empty, want non-empty")
	}

	page2, next2, _, err := s.EmailThreads().List(ctx, scope, next, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2))
	}
	if next2 != "" {
		t.Errorf("page2 next = %q, want empty", next2)
	}

	seen := map[string]int{}
	for _, it := range append(page1, page2...) {
		seen[it.PlatformThreadID]++
	}
	for _, id := range []string{"thr-1", "thr-2", "thr-3"} {
		if seen[id] != 1 {
			t.Errorf("thread %s seen %d times across pages, want exactly 1", id, seen[id])
		}
	}
}

func TestEmailThreadRepo_List_LimitClamp(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	seedThread(t, s, scope, "thr-1", "1@example.com", "support", "inbound.com")

	items, _, _, err := s.EmailThreads().List(ctx, scope, "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("len(items) = %d, want 1 (limit=0 should default, not return zero)", len(items))
	}
}
