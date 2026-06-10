package store

import (
	"context"
	"database/sql"

	"github.com/danmestas/notify/internal/domain"
)

// TranslationRepo persists per-locale message strings keyed by (EnvID, Locale,
// Key). Every method is Scope-first; translations carry environment_id so a
// locale's strings are invisible outside their owning org+env.
type TranslationRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, locale, key, value string) error
	Map(ctx context.Context, scope domain.Scope, locale string) (map[string]string, error)
}

type translationRepo struct {
	w *sql.DB
	r *sql.DB
}

// Upsert writes value for (scope, locale, key), updating the existing row in
// place on its (environment_id, locale, key) dedup key.
func (repo *translationRepo) Upsert(ctx context.Context, scope domain.Scope, locale, key, value string) error {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO translations (id, org_id, environment_id, locale, key, value)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, locale, key) DO UPDATE SET value=excluded.value`,
		newULID(), scope.OrgID, scope.EnvID, locale, key, value,
	)
	return err
}

// Map returns every key->value pair for the locale within scope. An empty
// result (no rows) yields an empty, non-nil map.
func (repo *translationRepo) Map(ctx context.Context, scope domain.Scope, locale string) (map[string]string, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT key, value FROM translations WHERE environment_id=? AND locale=?`,
		scope.EnvID, locale,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
