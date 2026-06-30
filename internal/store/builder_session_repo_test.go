package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestBuilderSessionRepo_CreateGetUpdateScope(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.BuilderSessions()

	scopeA := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	scopeB := domain.Scope{OrgID: "org-1", EnvID: "env-b"}

	sess, err := repo.Create(ctx, scopeA, "send a welcome email")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("Create returned empty id")
	}
	if sess.Status != BuilderSessionDraft {
		t.Fatalf("status = %q, want %q", sess.Status, BuilderSessionDraft)
	}
	if sess.Prompt != "send a welcome email" {
		t.Fatalf("prompt = %q", sess.Prompt)
	}

	// Get within scope succeeds.
	got, err := repo.Get(ctx, scopeA, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("Get id = %q, want %q", got.ID, sess.ID)
	}

	// Get from a different environment must not see the session.
	if _, err := repo.Get(ctx, scopeB, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env Get err = %v, want ErrNotFound", err)
	}

	// UpdateDraft stores the proposed draft and advances status.
	draft := json.RawMessage(`{"name":"Welcome","triggerIdentifier":"welcome","steps":[]}`)
	if err := repo.UpdateDraft(ctx, scopeA, sess.ID, draft, BuilderSessionDraft); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	got, err = repo.Get(ctx, scopeA, sess.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if string(got.DraftWorkflow) != string(draft) {
		t.Fatalf("draft = %s, want %s", got.DraftWorkflow, draft)
	}

	// Cross-env UpdateDraft must not touch the row.
	if err := repo.UpdateDraft(ctx, scopeB, sess.ID, json.RawMessage(`{}`), BuilderSessionConfirmed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env UpdateDraft err = %v, want ErrNotFound", err)
	}

	// SetStatus confirmed.
	if err := repo.SetStatus(ctx, scopeA, sess.ID, BuilderSessionConfirmed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, _ = repo.Get(ctx, scopeA, sess.ID)
	if got.Status != BuilderSessionConfirmed {
		t.Fatalf("status = %q, want confirmed", got.Status)
	}
}

func TestBuilderSessionRepo_Messages(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.BuilderSessions()
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	other := domain.Scope{OrgID: "org-1", EnvID: "env-b"}

	sess, err := repo.Create(ctx, scope, "prompt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.AppendMessage(ctx, scope, sess.ID, "assistant", "thinking", ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := repo.AppendMessage(ctx, scope, sess.ID, "tool", "addStep", `[{"name":"addStep"}]`); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	msgs, err := repo.Messages(ctx, scope, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Content != "thinking" || msgs[1].ToolCalls != `[{"name":"addStep"}]` {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	// Cross-env AppendMessage must be rejected.
	if err := repo.AppendMessage(ctx, other, sess.ID, "assistant", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-env AppendMessage err = %v, want ErrNotFound", err)
	}
}
