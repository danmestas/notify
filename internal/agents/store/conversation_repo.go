package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// ConversationRepo persists conversation threads keyed by
// (agent_id, integration_id, platform_thread_id). Every method is Scope-first.
type ConversationRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, c agentdomain.Conversation) (agentdomain.Conversation, error)
	Get(ctx context.Context, scope domain.Scope, id string) (agentdomain.Conversation, error)
}

type conversationRepo struct {
	w *sql.DB
	r *sql.DB
}

// Upsert inserts the conversation by its (agent_id, integration_id,
// platform_thread_id) dedup key. A fresh row mints a ULID id and timestamps
// with status 'open'; a re-upsert of an existing thread refreshes
// last_activity_at and keeps the original id. IntegrationID is stored as the
// empty string (not NULL) for channel-less threads so the dedup key matches.
// The DO UPDATE arm is scope-guarded so a conflict against another tenant's
// row never mutates it; the scoped read-back then surfaces ErrNotFound.
func (repo *conversationRepo) Upsert(ctx context.Context, scope domain.Scope, c agentdomain.Conversation) (agentdomain.Conversation, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if c.Status == "" {
		c.Status = "open"
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO conversations (id, org_id, environment_id, agent_id, integration_id,
		   platform_thread_id, subscriber_id, status, created_at, last_activity_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (agent_id, integration_id, platform_thread_id)
		 DO UPDATE SET last_activity_at=excluded.last_activity_at
		 WHERE org_id = excluded.org_id AND environment_id = excluded.environment_id`,
		corestore.NewULID(), scope.OrgID, scope.EnvID, c.AgentID, c.IntegrationID,
		c.PlatformThreadID, c.SubscriberID, c.Status, now, now,
	)
	if err != nil {
		return agentdomain.Conversation{}, err
	}

	var out agentdomain.Conversation
	err = repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, agent_id, integration_id, platform_thread_id,
		   subscriber_id, status, created_at, last_activity_at
		 FROM conversations
		 WHERE org_id = ? AND environment_id = ? AND agent_id = ? AND integration_id = ? AND platform_thread_id = ?`,
		scope.OrgID, scope.EnvID, c.AgentID, c.IntegrationID, c.PlatformThreadID,
	).Scan(&out.ID, &out.OrgID, &out.EnvID, &out.AgentID, &out.IntegrationID, &out.PlatformThreadID,
		&out.SubscriberID, &out.Status, &out.CreatedAt, &out.LastActivityAt)
	if errors.Is(err, sql.ErrNoRows) {
		return agentdomain.Conversation{}, ErrNotFound
	}
	if err != nil {
		return agentdomain.Conversation{}, err
	}
	return out, nil
}

func (repo *conversationRepo) Get(ctx context.Context, scope domain.Scope, id string) (agentdomain.Conversation, error) {
	var out agentdomain.Conversation
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, agent_id, integration_id, platform_thread_id,
		   subscriber_id, status, created_at, last_activity_at
		 FROM conversations WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&out.ID, &out.OrgID, &out.EnvID, &out.AgentID, &out.IntegrationID, &out.PlatformThreadID,
		&out.SubscriberID, &out.Status, &out.CreatedAt, &out.LastActivityAt)
	if errors.Is(err, sql.ErrNoRows) {
		return agentdomain.Conversation{}, ErrNotFound
	}
	if err != nil {
		return agentdomain.Conversation{}, err
	}
	return out, nil
}
