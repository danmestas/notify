package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// ApiKeyRepo binds an opaque key hash to an already-minted Scope. Create takes
// the Scope its key authorises; GetByHash resolves a hash back to that Scope
// (the only lookup whose input is not yet scoped — it is how a request earns
// its Scope).
type ApiKeyRepo interface {
	Create(ctx context.Context, scope domain.Scope, key domain.APIKey) (domain.APIKey, error)
	GetByHash(ctx context.Context, hash string) (domain.APIKey, domain.Scope, error)
	// List returns all API keys for the scope's env. Used by the console to
	// surface and decrypt the active key for display.
	List(ctx context.Context, scope domain.Scope) ([]domain.APIKey, error)
	// DeleteByScope removes every API key belonging to scope in one statement.
	// RotateKey calls this and then Create within a single write transaction.
	DeleteByScope(ctx context.Context, scope domain.Scope) error
}

type apiKeyRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *apiKeyRepo) Create(ctx context.Context, scope domain.Scope, key domain.APIKey) (domain.APIKey, error) {
	key.OrgID = scope.OrgID
	key.EnvID = scope.EnvID
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO api_keys (hash, environment_id, org_id, enc_key, user_id)
		 VALUES (?, ?, ?, ?, ?)`,
		key.Hash, key.EnvID, key.OrgID, key.EncKey, key.UserID,
	)
	if err != nil {
		return domain.APIKey{}, err
	}
	return key, nil
}

func (repo *apiKeyRepo) GetByHash(ctx context.Context, hash string) (domain.APIKey, domain.Scope, error) {
	var key domain.APIKey
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT hash, environment_id, org_id, enc_key, user_id FROM api_keys WHERE hash = ?`,
		hash,
	).Scan(&key.Hash, &key.EnvID, &key.OrgID, &key.EncKey, &key.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.APIKey{}, domain.Scope{}, ErrNotFound
	}
	if err != nil {
		return domain.APIKey{}, domain.Scope{}, err
	}
	return key, domain.Scope{OrgID: key.OrgID, EnvID: key.EnvID}, nil
}

func (repo *apiKeyRepo) List(ctx context.Context, scope domain.Scope) ([]domain.APIKey, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT hash, environment_id, org_id, enc_key, user_id
		 FROM api_keys WHERE org_id = ? AND environment_id = ?`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var keys []domain.APIKey
	for rows.Next() {
		var k domain.APIKey
		if err := rows.Scan(&k.Hash, &k.EnvID, &k.OrgID, &k.EncKey, &k.UserID); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (repo *apiKeyRepo) DeleteByScope(ctx context.Context, scope domain.Scope) error {
	_, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE org_id = ? AND environment_id = ?`,
		scope.OrgID, scope.EnvID,
	)
	return err
}
