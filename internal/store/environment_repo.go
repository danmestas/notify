package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// EnvironmentRepo is a tenancy root: Create mints the Scope (it takes the
// parent orgID, not a Scope), while Get is scoped by org + env id.
type EnvironmentRepo interface {
	Create(ctx context.Context, orgID string, env domain.Environment) (domain.Environment, domain.Scope, error)
	Get(ctx context.Context, scope domain.Scope) (domain.Environment, error)
}

type envRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *envRepo) Create(ctx context.Context, orgID string, env domain.Environment) (domain.Environment, domain.Scope, error) {
	env.OrgID = orgID
	if env.ID == "" {
		env.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO environments (id, org_id, name, identifier, type, bridge_url, parent_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		env.ID, env.OrgID, env.Name, env.Identifier, env.Type, env.BridgeURL, env.ParentID,
	)
	if err != nil {
		return domain.Environment{}, domain.Scope{}, err
	}
	return env, domain.Scope{OrgID: orgID, EnvID: env.ID}, nil
}

func (repo *envRepo) Get(ctx context.Context, scope domain.Scope) (domain.Environment, error) {
	var env domain.Environment
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, name, identifier, type, bridge_url, parent_id
		 FROM environments WHERE org_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID,
	).Scan(&env.ID, &env.OrgID, &env.Name, &env.Identifier, &env.Type, &env.BridgeURL, &env.ParentID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Environment{}, ErrNotFound
	}
	if err != nil {
		return domain.Environment{}, err
	}
	return env, nil
}
