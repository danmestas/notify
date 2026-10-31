package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestChatPollCursorRepo_GetMissingReturnsEmpty(t *testing.T) {
	s := openTemp(t)
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	got, err := s.ChatPollCursors().Get(context.Background(), scope, "bluesky")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Fatalf("Get on missing key = %q, want empty", got)
	}
}

func TestChatPollCursorRepo_SetGetOverwriteAndIsolation(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	other := domain.Scope{OrgID: "o2", EnvID: "e1"}
	repo := s.ChatPollCursors()

	if err := repo.Set(ctx, scope, "telegram", "12"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := repo.Get(ctx, scope, "telegram")
	if err != nil || got != "12" {
		t.Fatalf("Get = %q, %v; want 12", got, err)
	}
	if err := repo.Set(ctx, scope, "telegram", "99"); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if got, _ := repo.Get(ctx, scope, "telegram"); got != "99" {
		t.Fatalf("after overwrite Get = %q, want 99", got)
	}
	if got, _ := repo.Get(ctx, scope, "mastodon"); got != "" {
		t.Fatalf("other provider = %q, want empty", got)
	}
	if got, _ := repo.Get(ctx, other, "telegram"); got != "" {
		t.Fatalf("other scope = %q, want empty", got)
	}
}
