package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// ChatThread is one chat-origin conversation binding: it maps a conversation
// thread (platform_thread_id) to the provider and the chat_id the user
// messaged from (Telegram chat_id / Discord channel_id), plus the responder
// agent. The chat two-way loop reads it two ways: by (provider, chat_id) to
// route inbound chat onto the right conversation, and by platform_thread_id to
// send the agent's reply back into the originating chat.
type ChatThread struct {
	ID               string `db:"id"`
	OrgID            string `db:"org_id"`
	EnvID            string `db:"environment_id"`
	AgentIdentifier  string `db:"agent_identifier"`
	PlatformThreadID string `db:"platform_thread_id"`
	SubscriberID     string `db:"subscriber_id"`
	Provider         string `db:"provider"`
	ChatID           string `db:"chat_id"`
	CreatedAt        string `db:"created_at"`
	LastActivityAt   string `db:"last_activity_at"`
}

// ChatThreadRepo persists chat-origin conversation bindings, scoped to
// (org_id, environment_id). The inbound dedup key is (provider, chat_id); the
// reverse (ReplyHook) lookup is by platform_thread_id.
type ChatThreadRepo interface {
	// Upsert inserts the binding for its (org, env, provider, chat_id) key, or
	// refreshes last_activity_at (and the mutable agent/thread/subscriber
	// fields) on a re-bind of an existing chat. The original id is kept.
	Upsert(ctx context.Context, scope domain.Scope, t ChatThread) (ChatThread, error)
	// GetByChat reads the binding for a (provider, chat_id) within scope,
	// without creating one. ErrNotFound when absent. Used by inbound routing.
	GetByChat(ctx context.Context, scope domain.Scope, provider, chatID string) (ChatThread, error)
	// GetByThread reads the binding for a conversation's platform_thread_id
	// within scope. ErrNotFound when absent. Used by the ReplyHook reverse
	// lookup.
	GetByThread(ctx context.Context, scope domain.Scope, platformThreadID string) (ChatThread, error)
}

type chatThreadRepo struct {
	w *sql.DB
	r *sql.DB
}

// Upsert inserts the binding by its (org_id, environment_id, provider, chat_id)
// key. A fresh row mints a ULID id and timestamps; a re-upsert refreshes the
// mutable fields and last_activity_at while keeping the original id. The DO
// UPDATE arm is scope-guarded so a key collision against another tenant never
// mutates their row.
func (repo *chatThreadRepo) Upsert(ctx context.Context, scope domain.Scope, t ChatThread) (ChatThread, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := newULID()
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO chat_threads (id, org_id, environment_id, agent_identifier,
		   platform_thread_id, subscriber_id, provider, chat_id,
		   created_at, last_activity_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (org_id, environment_id, provider, chat_id)
		 DO UPDATE SET agent_identifier=excluded.agent_identifier,
		   platform_thread_id=excluded.platform_thread_id,
		   subscriber_id=excluded.subscriber_id,
		   last_activity_at=excluded.last_activity_at
		 WHERE org_id = excluded.org_id AND environment_id = excluded.environment_id`,
		id, scope.OrgID, scope.EnvID, t.AgentIdentifier, t.PlatformThreadID,
		t.SubscriberID, t.Provider, t.ChatID, now, now,
	)
	if err != nil {
		return ChatThread{}, err
	}
	return repo.GetByChat(ctx, scope, t.Provider, t.ChatID)
}

// GetByChat reads the binding for (provider, chatID) within scope.
func (repo *chatThreadRepo) GetByChat(ctx context.Context, scope domain.Scope, provider, chatID string) (ChatThread, error) {
	return repo.queryOne(
		ctx,
		`SELECT id, org_id, environment_id, agent_identifier, platform_thread_id,
		   subscriber_id, provider, chat_id, created_at, last_activity_at
		 FROM chat_threads
		 WHERE org_id = ? AND environment_id = ? AND provider = ? AND chat_id = ?`,
		scope.OrgID, scope.EnvID, provider, chatID,
	)
}

// GetByThread reads the binding for platformThreadID within scope.
func (repo *chatThreadRepo) GetByThread(ctx context.Context, scope domain.Scope, platformThreadID string) (ChatThread, error) {
	return repo.queryOne(
		ctx,
		`SELECT id, org_id, environment_id, agent_identifier, platform_thread_id,
		   subscriber_id, provider, chat_id, created_at, last_activity_at
		 FROM chat_threads
		 WHERE org_id = ? AND environment_id = ? AND platform_thread_id = ?`,
		scope.OrgID, scope.EnvID, platformThreadID,
	)
}

func (repo *chatThreadRepo) queryOne(ctx context.Context, query string, args ...any) (ChatThread, error) {
	var out ChatThread
	err := repo.r.QueryRowContext(ctx, query, args...).Scan(
		&out.ID, &out.OrgID, &out.EnvID, &out.AgentIdentifier, &out.PlatformThreadID,
		&out.SubscriberID, &out.Provider, &out.ChatID, &out.CreatedAt, &out.LastActivityAt,
	)
	if err == sql.ErrNoRows {
		return ChatThread{}, ErrNotFound
	}
	if err != nil {
		return ChatThread{}, err
	}
	return out, nil
}
