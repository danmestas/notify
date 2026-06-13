package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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
	// Create inserts a new layout. Returns ErrConflict when (environment_id,
	// key) already exists. Unlike Upsert it never overwrites an existing row —
	// it is the console/API create path, which must surface duplicate keys.
	Create(ctx context.Context, scope domain.Scope, key, name, content string) (domain.Layout, error)
	// List returns the scope's layouts ordered by key ASC (keyset pagination).
	// afterKey="" starts from the beginning; otherwise rows with key > afterKey
	// are returned. Up to limit rows come back plus nextKey — the key to pass as
	// afterKey for the following page, or "" when no more rows remain.
	List(ctx context.Context, scope domain.Scope, afterKey string, limit int) ([]domain.Layout, string, error)
	// Delete removes the layout for (scope, key). Returns ErrNotFound when no
	// row matches.
	Delete(ctx context.Context, scope domain.Scope, key string) error
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

// Create inserts a new layout for (scope, key). A duplicate (environment_id,
// key) maps to ErrConflict; Create never updates an existing row.
func (repo *layoutRepo) Create(ctx context.Context, scope domain.Scope, key, name, content string) (domain.Layout, error) {
	l := domain.Layout{
		ID:      newULID(),
		OrgID:   scope.OrgID,
		EnvID:   scope.EnvID,
		Key:     key,
		Name:    name,
		Content: content,
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO layouts (id, org_id, environment_id, key, name, content)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		l.ID, l.OrgID, l.EnvID, l.Key, l.Name, l.Content,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return domain.Layout{}, ErrConflict
		}
		return domain.Layout{}, err
	}
	return l, nil
}

// List returns the scope's layouts ordered by key ASC. It fetches limit+1 rows
// to detect a following page: when the surplus row is present, nextKey is the
// last returned row's key and the surplus is trimmed.
func (repo *layoutRepo) List(ctx context.Context, scope domain.Scope, afterKey string, limit int) ([]domain.Layout, string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, key, name, content
		 FROM layouts
		 WHERE environment_id=? AND (?='' OR key > ?)
		 ORDER BY key ASC
		 LIMIT ?`,
		scope.EnvID, afterKey, afterKey, limit+1,
	)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()

	out := make([]domain.Layout, 0, limit)
	for rows.Next() {
		var l domain.Layout
		if err := rows.Scan(&l.ID, &l.OrgID, &l.EnvID, &l.Key, &l.Name, &l.Content); err != nil {
			return nil, "", err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	nextKey := ""
	if len(out) > limit {
		out = out[:limit]
		nextKey = out[len(out)-1].Key
	}
	return out, nextKey, nil
}

// Delete removes the layout for (scope, key), returning ErrNotFound when no row
// matches.
func (repo *layoutRepo) Delete(ctx context.Context, scope domain.Scope, key string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM layouts WHERE environment_id=? AND key=?`,
		scope.EnvID, key,
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
