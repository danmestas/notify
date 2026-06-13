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

	// ListLocales returns all distinct locale codes for scope, ordered ASC.
	ListLocales(ctx context.Context, scope domain.Scope) ([]string, error)

	// Delete removes (scope, locale, key). Returns ErrNotFound when no row matches.
	Delete(ctx context.Context, scope domain.Scope, locale, key string) error

	// DeleteLocale removes ALL keys for (scope, locale). Used by the two-step
	// locale delete. Returns ErrNotFound when the locale has zero rows.
	DeleteLocale(ctx context.Context, scope domain.Scope, locale string) error

	// LocaleStats returns key count for every locale in scope, ordered ASC.
	LocaleStats(ctx context.Context, scope domain.Scope) ([]LocaleStat, error)

	// BulkReplace atomically replaces the entire key map for (scope, locale).
	// It deletes all existing rows then inserts pairs. If pairs is empty the
	// locale is cleared. Runs in a transaction.
	BulkReplace(ctx context.Context, scope domain.Scope, locale string, pairs map[string]string) error
}

// LocaleStat is one row of TranslationRepo.LocaleStats: a locale and the
// number of keys it carries within a scope.
type LocaleStat struct {
	Locale   string
	KeyCount int
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

// ListLocales returns all distinct locale codes for scope, ordered ASC.
func (repo *translationRepo) ListLocales(ctx context.Context, scope domain.Scope) ([]string, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT DISTINCT locale FROM translations WHERE environment_id=? ORDER BY locale ASC`,
		scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0)
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Delete removes (scope, locale, key). Returns ErrNotFound when no row matches.
func (repo *translationRepo) Delete(ctx context.Context, scope domain.Scope, locale, key string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM translations WHERE environment_id=? AND locale=? AND key=?`,
		scope.EnvID, locale, key,
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

// DeleteLocale removes ALL keys for (scope, locale). Returns ErrNotFound when
// the locale has zero rows.
func (repo *translationRepo) DeleteLocale(ctx context.Context, scope domain.Scope, locale string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM translations WHERE environment_id=? AND locale=?`,
		scope.EnvID, locale,
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

// LocaleStats returns key count for every locale in scope, ordered ASC.
func (repo *translationRepo) LocaleStats(ctx context.Context, scope domain.Scope) ([]LocaleStat, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT locale, COUNT(*) FROM translations WHERE environment_id=? GROUP BY locale ORDER BY locale ASC`,
		scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]LocaleStat, 0)
	for rows.Next() {
		var s LocaleStat
		if err := rows.Scan(&s.Locale, &s.KeyCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BulkReplace atomically replaces the entire key map for (scope, locale): it
// deletes all existing rows for the locale then inserts pairs, all in one
// transaction. An empty pairs map clears the locale.
func (repo *translationRepo) BulkReplace(ctx context.Context, scope domain.Scope, locale string, pairs map[string]string) error {
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM translations WHERE environment_id=? AND locale=?`,
		scope.EnvID, locale,
	); err != nil {
		return err
	}

	for k, v := range pairs {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO translations (id, org_id, environment_id, locale, key, value)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			newULID(), scope.OrgID, scope.EnvID, locale, k, v,
		); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
