package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// LayoutRepo persists reusable content wrappers keyed by (EnvID, Key). A layout
// holds a content template carrying a {{content}} placeholder; a channel step
// referencing the layout's key injects its rendered body there. Every method is
// Scope-first; layouts carry environment_id so a key is invisible outside its
// owning org+env.
type LayoutRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, key, name, content string) error
	Get(ctx context.Context, scope domain.Scope, key string) (domain.Layout, error)
}

type layoutRepo struct {
	w *sql.DB
	r *sql.DB
}

// Upsert writes the layout for (scope, key), updating name+content in place on
// its (environment_id, key) dedup key.
func (repo *layoutRepo) Upsert(ctx context.Context, scope domain.Scope, key, name, content string) error {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO layouts (id, org_id, environment_id, key, name, content)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, key) DO UPDATE SET name=excluded.name, content=excluded.content`,
		newULID(), scope.OrgID, scope.EnvID, key, name, content,
	)
	return err
}

// Get returns the layout for (scope, key), or ErrNotFound when no row matches.
func (repo *layoutRepo) Get(ctx context.Context, scope domain.Scope, key string) (domain.Layout, error) {
	var l domain.Layout
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, key, name, content
		 FROM layouts WHERE environment_id=? AND key=?`,
		scope.EnvID, key,
	).Scan(&l.ID, &l.OrgID, &l.EnvID, &l.Key, &l.Name, &l.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Layout{}, ErrNotFound
	}
	if err != nil {
		return domain.Layout{}, err
	}
	return l, nil
}
