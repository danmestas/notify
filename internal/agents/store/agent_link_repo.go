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

// AgentLinkRepo persists agent <-> channel-integration links keyed by
// (agent_id, integration_id). Every method is Scope-first.
type AgentLinkRepo interface {
	Link(ctx context.Context, scope domain.Scope, l agentdomain.AgentLink) (agentdomain.AgentLink, error)
	ListByAgent(ctx context.Context, scope domain.Scope, agentID string) ([]agentdomain.AgentLink, error)
	Unlink(ctx context.Context, scope domain.Scope, agentID, integrationID string) error
}

type agentLinkRepo struct {
	w *sql.DB
	r *sql.DB
}

// Link upserts by the (agent_id, integration_id) dedup key. A fresh link mints
// a ULID id and connected_at; re-linking updates config in place and keeps the
// original id and timestamp. The DO UPDATE arm is scope-guarded so a conflict
// against another tenant's row never mutates it; the scoped read-back then
// surfaces ErrNotFound to the caller.
func (repo *agentLinkRepo) Link(ctx context.Context, scope domain.Scope, l agentdomain.AgentLink) (agentdomain.AgentLink, error) {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO agent_links (id, org_id, environment_id, agent_id, integration_id, connected_at, config)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (agent_id, integration_id) DO UPDATE SET config=excluded.config
		 WHERE org_id = excluded.org_id AND environment_id = excluded.environment_id`,
		corestore.NewULID(), scope.OrgID, scope.EnvID, l.AgentID, l.IntegrationID,
		timestamp.Now(), string(l.Config),
	)
	if err != nil {
		return agentdomain.AgentLink{}, err
	}
	return repo.get(ctx, scope, l.AgentID, l.IntegrationID)
}

func (repo *agentLinkRepo) get(ctx context.Context, scope domain.Scope, agentID, integrationID string) (agentdomain.AgentLink, error) {
	var (
		l      agentdomain.AgentLink
		config string
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, agent_id, integration_id, connected_at, COALESCE(config, '')
		 FROM agent_links
		 WHERE org_id = ? AND environment_id = ? AND agent_id = ? AND integration_id = ?`,
		scope.OrgID, scope.EnvID, agentID, integrationID,
	).Scan(&l.ID, &l.OrgID, &l.EnvID, &l.AgentID, &l.IntegrationID, &l.ConnectedAt, &config)
	if errors.Is(err, sql.ErrNoRows) {
		return agentdomain.AgentLink{}, ErrNotFound
	}
	if err != nil {
		return agentdomain.AgentLink{}, err
	}
	if config != "" {
		l.Config = []byte(config)
	}
	return l, nil
}

func (repo *agentLinkRepo) ListByAgent(ctx context.Context, scope domain.Scope, agentID string) ([]agentdomain.AgentLink, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, agent_id, integration_id, connected_at, COALESCE(config, '')
		 FROM agent_links
		 WHERE org_id = ? AND environment_id = ? AND agent_id = ?
		 ORDER BY connected_at, id`,
		scope.OrgID, scope.EnvID, agentID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []agentdomain.AgentLink
	for rows.Next() {
		var (
			l      agentdomain.AgentLink
			config string
		)
		if err := rows.Scan(&l.ID, &l.OrgID, &l.EnvID, &l.AgentID, &l.IntegrationID, &l.ConnectedAt, &config); err != nil {
			return nil, err
		}
		if config != "" {
			l.Config = []byte(config)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Unlink removes the (agentID, integrationID) link. A missing link surfaces
// ErrNotFound.
func (repo *agentLinkRepo) Unlink(ctx context.Context, scope domain.Scope, agentID, integrationID string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM agent_links
		 WHERE org_id = ? AND environment_id = ? AND agent_id = ? AND integration_id = ?`,
		scope.OrgID, scope.EnvID, agentID, integrationID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
