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
	// List returns all environments for an org ordered by insertion order (rowid).
	List(ctx context.Context, orgID string) ([]domain.Environment, error)
	// Update renames an environment. Returns ErrNotFound when no matching row exists.
	Update(ctx context.Context, orgID, id, name string) error
	// Delete hard-deletes an environment row. The caller must enforce the
	// "not the last/current environment" guard before invoking this.
	Delete(ctx context.Context, orgID, id string) error
	// CountByOrg returns the number of environments owned by orgID.
	// Used by the delete guard to prevent destroying the last environment.
	CountByOrg(ctx context.Context, orgID string) (int, error)
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

func (repo *envRepo) List(ctx context.Context, orgID string) ([]domain.Environment, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, name, identifier, type, bridge_url, parent_id
		 FROM environments WHERE org_id = ? ORDER BY rowid ASC`,
		orgID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var envs []domain.Environment
	for rows.Next() {
		var env domain.Environment
		if err := rows.Scan(&env.ID, &env.OrgID, &env.Name, &env.Identifier,
			&env.Type, &env.BridgeURL, &env.ParentID); err != nil {
			return nil, err
		}
		envs = append(envs, env)
	}
	return envs, rows.Err()
}

func (repo *envRepo) Update(ctx context.Context, orgID, id, name string) error {
	result, err := repo.w.ExecContext(
		ctx,
		`UPDATE environments SET name = ? WHERE org_id = ? AND id = ?`,
		name, orgID, id,
	)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (repo *envRepo) Delete(ctx context.Context, orgID, id string) error {
	result, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM environments WHERE org_id = ? AND id = ?`,
		orgID, id,
	)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (repo *envRepo) CountByOrg(ctx context.Context, orgID string) (int, error) {
	var count int
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM environments WHERE org_id = ?`,
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}
