package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// A2AClientKeyRepo persists A2A client keys per environment. Every method is
// Scope-first EXCEPT GetBySecretHash and Touch: the secret hash is the
// credential, so that lookup returns the owning row (and thus its scope)
// rather than being checked against a caller-supplied scope -- the same
// documented unscoped read as StreamSources().GetBySecretHash.
type A2AClientKeyRepo interface {
	Create(ctx context.Context, scope domain.Scope, key domain.A2AClientKey) (domain.A2AClientKey, error)
	// List returns the scope's keys (revoked included) ordered by created_at
	// then id.
	List(ctx context.Context, scope domain.Scope) ([]domain.A2AClientKey, error)
	// GetBySecretHash resolves the key whose hash matches, revoked or not, so
	// the caller can decide how to answer. ErrNotFound on a miss.
	GetBySecretHash(ctx context.Context, hash string) (domain.A2AClientKey, error)
	// Revoke stamps revoked_at. ErrNotFound when the id is absent in the scope
	// or already revoked.
	Revoke(ctx context.Context, scope domain.Scope, id, at string) error
	// Touch records last_used_at, keyed by id alone (best-effort hot path).
	Touch(ctx context.Context, id, at string) error
}

type a2aClientKeyRepo struct {
	w *sql.DB
	r *sql.DB
}

// A2AClientKeys vends the A2A client key repository.
func (s *Store) A2AClientKeys() A2AClientKeyRepo {
	return &a2aClientKeyRepo{w: s.writer, r: s.reader}
}

const a2aKeyCols = `id, org_id, environment_id, name, subscriber_id, secret_hash, created_at, last_used_at, revoked_at`

func scanA2AKey(sc interface{ Scan(...any) error }) (domain.A2AClientKey, error) {
	var k domain.A2AClientKey
	err := sc.Scan(&k.ID, &k.OrgID, &k.EnvID, &k.Name, &k.SubscriberID, &k.SecretHash, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	return k, err
}

func (repo *a2aClientKeyRepo) Create(ctx context.Context, scope domain.Scope, key domain.A2AClientKey) (domain.A2AClientKey, error) {
	key.OrgID = scope.OrgID
	key.EnvID = scope.EnvID
	if key.ID == "" {
		key.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO a2a_client_keys (`+a2aKeyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.OrgID, key.EnvID, key.Name, key.SubscriberID, key.SecretHash,
		key.CreatedAt, key.LastUsedAt, key.RevokedAt,
	)
	if err != nil {
		return domain.A2AClientKey{}, err
	}
	return key, nil
}

func (repo *a2aClientKeyRepo) List(ctx context.Context, scope domain.Scope) ([]domain.A2AClientKey, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+a2aKeyCols+` FROM a2a_client_keys
		 WHERE org_id = ? AND environment_id = ? ORDER BY created_at ASC, id ASC`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.A2AClientKey
	for rows.Next() {
		k, err := scanA2AKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (repo *a2aClientKeyRepo) GetBySecretHash(ctx context.Context, hash string) (domain.A2AClientKey, error) {
	k, err := scanA2AKey(repo.r.QueryRowContext(ctx,
		`SELECT `+a2aKeyCols+` FROM a2a_client_keys WHERE secret_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.A2AClientKey{}, ErrNotFound
	}
	return k, err
}

func (repo *a2aClientKeyRepo) Revoke(ctx context.Context, scope domain.Scope, id, at string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE a2a_client_keys SET revoked_at = ?
		 WHERE id = ? AND org_id = ? AND environment_id = ? AND revoked_at = ''`,
		at, id, scope.OrgID, scope.EnvID,
	)
	return affectedOne(res, err)
}

func (repo *a2aClientKeyRepo) Touch(ctx context.Context, id, at string) error {
	_, err := repo.w.ExecContext(ctx, `UPDATE a2a_client_keys SET last_used_at = ? WHERE id = ?`, at, id)
	return err
}
