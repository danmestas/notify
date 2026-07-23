package store

import (
	"context"
	"database/sql"
	"errors"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// conversationListDefaultLimit and conversationListMaxLimit cap ListByAgent pages.
const (
	conversationListDefaultLimit = 50
	conversationListMaxLimit     = 200
)

// ConversationRepo persists conversation threads keyed by
// (agent_id, integration_id, platform_thread_id). Every method is Scope-first.
type ConversationRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, c agentdomain.Conversation) (agentdomain.Conversation, error)
	Get(ctx context.Context, scope domain.Scope, id string) (agentdomain.Conversation, error)
	// GetByThread reads a conversation by its dedup key without creating one
	// (the read-side counterpart of Upsert). ErrNotFound when absent.
	GetByThread(ctx context.Context, scope domain.Scope, agentID, integrationID, platformThreadID string) (agentdomain.Conversation, error)
	// ListByAgent returns conversations for agentID ordered by last_activity_at
	// DESC, id DESC. limit ≤ 0 → default 50, hard cap 200. Pass before="" for
	// the first page; subsequent pages pass the id of the last row on the
	// previous page as the keyset anchor.
	ListByAgent(ctx context.Context, scope domain.Scope, agentID string, limit int, before string) ([]agentdomain.Conversation, error)
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
	now := timestamp.Now()
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

// ListByAgent returns a keyset-paginated page of conversations for agentID
// within scope, ordered by last_activity_at DESC, id DESC. Passing before=""
// returns the first page; subsequent pages pass the id of the last row on the
// previous page. limit ≤ 0 → default 50, hard cap 200.
func (repo *conversationRepo) ListByAgent(ctx context.Context, scope domain.Scope, agentID string, limit int, before string) ([]agentdomain.Conversation, error) {
	if limit <= 0 {
		limit = conversationListDefaultLimit
	}
	if limit > conversationListMaxLimit {
		limit = conversationListMaxLimit
	}

	const cols = `id, org_id, environment_id, agent_id, integration_id, platform_thread_id,
		   subscriber_id, status, created_at, last_activity_at`

	var (
		rows *sql.Rows
		err  error
	)
	if before == "" {
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT `+cols+`
			 FROM conversations
			 WHERE org_id = ? AND environment_id = ? AND agent_id = ?
			 ORDER BY last_activity_at DESC, id DESC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, agentID, limit,
		)
	} else {
		// Fetch the anchor row's last_activity_at so we can use both columns
		// for a correct DESC keyset (ties on last_activity_at break on id DESC).
		var anchorActivityAt string
		if err := repo.r.QueryRowContext(
			ctx,
			`SELECT last_activity_at FROM conversations WHERE org_id = ? AND environment_id = ? AND id = ?`,
			scope.OrgID, scope.EnvID, before,
		).Scan(&anchorActivityAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Anchor is gone or belongs to another scope; treat as first page.
				rows, err = repo.r.QueryContext(
					ctx,
					`SELECT `+cols+`
					 FROM conversations
					 WHERE org_id = ? AND environment_id = ? AND agent_id = ?
					 ORDER BY last_activity_at DESC, id DESC
					 LIMIT ?`,
					scope.OrgID, scope.EnvID, agentID, limit,
				)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		} else {
			rows, err = repo.r.QueryContext(
				ctx,
				`SELECT `+cols+`
				 FROM conversations
				 WHERE org_id = ? AND environment_id = ? AND agent_id = ?
				   AND (last_activity_at < ? OR (last_activity_at = ? AND id < ?))
				 ORDER BY last_activity_at DESC, id DESC
				 LIMIT ?`,
				scope.OrgID, scope.EnvID, agentID,
				anchorActivityAt, anchorActivityAt, before,
				limit,
			)
			if err != nil {
				return nil, err
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return []agentdomain.Conversation{}, nil
	}
	defer func() { _ = rows.Close() }()

	var out []agentdomain.Conversation
	for rows.Next() {
		var c agentdomain.Conversation
		if err := rows.Scan(&c.ID, &c.OrgID, &c.EnvID, &c.AgentID, &c.IntegrationID,
			&c.PlatformThreadID, &c.SubscriberID, &c.Status, &c.CreatedAt, &c.LastActivityAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return []agentdomain.Conversation{}, nil
	}
	return out, nil
}

func (repo *conversationRepo) GetByThread(ctx context.Context, scope domain.Scope, agentID, integrationID, platformThreadID string) (agentdomain.Conversation, error) {
	var out agentdomain.Conversation
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, agent_id, integration_id, platform_thread_id,
		   subscriber_id, status, created_at, last_activity_at
		 FROM conversations
		 WHERE org_id = ? AND environment_id = ? AND agent_id = ? AND integration_id = ? AND platform_thread_id = ?`,
		scope.OrgID, scope.EnvID, agentID, integrationID, platformThreadID,
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
