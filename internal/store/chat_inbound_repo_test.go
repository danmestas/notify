package store

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

func TestChatInboundRepo_MarkHandledOnce(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.ChatInbound()

	first, err := repo.MarkHandled(ctx, scope, "bluesky", "at://x/1")
	if err != nil || !first {
		t.Fatalf("first MarkHandled = %v, %v; want true", first, err)
	}
	again, err := repo.MarkHandled(ctx, scope, "bluesky", "at://x/1")
	if err != nil || again {
		t.Fatalf("second MarkHandled = %v, %v; want false", again, err)
	}
	for name, tc := range map[string]struct {
		scope    domain.Scope
		provider string
	}{
		"other provider": {scope, "mastodon"},
		"other scope":    {domain.Scope{OrgID: "o2", EnvID: "e1"}, "bluesky"},
	} {
		got, err := repo.MarkHandled(ctx, tc.scope, tc.provider, "at://x/1")
		if err != nil || !got {
			t.Fatalf("%s: MarkHandled = %v, %v; want true", name, got, err)
		}
	}
}

func TestChatInboundRepo_PurgeOlderThan(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	repo := s.ChatInbound()

	if _, err := repo.MarkHandled(ctx, scope, "telegram", "1"); err != nil {
		t.Fatalf("MarkHandled: %v", err)
	}
	n, err := repo.PurgeOlderThan(ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("purge before = %d, %v; want 0", n, err)
	}
	n, err = repo.PurgeOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge after = %d, %v; want 1", n, err)
	}
	if fresh, _ := repo.MarkHandled(ctx, scope, "telegram", "1"); !fresh {
		t.Fatalf("purged claim should be claimable again")
	}
}
