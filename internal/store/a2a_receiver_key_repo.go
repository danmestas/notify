package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// A2AReceiverKeyRepo persists A2A receiver keys per agent. Every method is
// Scope-first EXCEPT GetBySecretHash and Touch: the secret hash is the
// credential, so that lookup returns the owning row (and thus its scope)
// rather than being checked against a caller-supplied scope -- the same
// documented unscoped read as StreamSources().GetBySecretHash.
type A2AReceiverKeyRepo interface {
	Create(ctx context.Context, scope domain.Scope, key domain.A2AReceiverKey) (domain.A2AReceiverKey, error)
	// List returns one agent's keys in the scope (revoked included) ordered by
	// created_at then id.
	List(ctx context.Context, scope domain.Scope, agentID string) ([]domain.A2AReceiverKey, error)
	// GetBySecretHash resolves the key whose hash matches, revoked or not, so
	// the caller can decide how to answer. ErrNotFound on a miss.
	GetBySecretHash(ctx context.Context, hash string) (domain.A2AReceiverKey, error)
	// Revoke stamps revoked_at. ErrNotFound when the id is absent in the scope
	// or already revoked.
	Revoke(ctx context.Context, scope domain.Scope, id, at string) error
	// Touch records last_used_at, keyed by id alone (best-effort hot path).
	Touch(ctx context.Context, id, at string) error
}

type a2aReceiverKeyRepo struct {
	w *sql.DB
	r *sql.DB
}

// A2AReceiverKeys vends the A2A receiver key repository.
func (s *Store) A2AReceiverKeys() A2AReceiverKeyRepo {
	return &a2aReceiverKeyRepo{w: s.writer, r: s.reader}
}

const a2aReceiverCols = `id, org_id, environment_id, name, agent_id, secret_hash, created_at, last_used_at, revoked_at`

func scanA2AReceiverKey(sc interface{ Scan(...any) error }) (domain.A2AReceiverKey, error) {
	var k domain.A2AReceiverKey
	err := sc.Scan(&k.ID, &k.OrgID, &k.EnvID, &k.Name, &k.AgentID, &k.SecretHash, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	return k, err
}

func (repo *a2aReceiverKeyRepo) Create(ctx context.Context, scope domain.Scope, key domain.A2AReceiverKey) (domain.A2AReceiverKey, error) {
	key.OrgID = scope.OrgID
	key.EnvID = scope.EnvID
	if key.ID == "" {
		key.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO a2a_receiver_keys (`+a2aReceiverCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.OrgID, key.EnvID, key.Name, key.AgentID, key.SecretHash,
		key.CreatedAt, key.LastUsedAt, key.RevokedAt,
	)
	if err != nil {
		return domain.A2AReceiverKey{}, err
	}
	return key, nil
}

func (repo *a2aReceiverKeyRepo) List(ctx context.Context, scope domain.Scope, agentID string) ([]domain.A2AReceiverKey, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+a2aReceiverCols+` FROM a2a_receiver_keys
		 WHERE org_id = ? AND environment_id = ? AND agent_id = ? ORDER BY created_at ASC, id ASC`,
		scope.OrgID, scope.EnvID, agentID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.A2AReceiverKey
	for rows.Next() {
		k, err := scanA2AReceiverKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (repo *a2aReceiverKeyRepo) GetBySecretHash(ctx context.Context, hash string) (domain.A2AReceiverKey, error) {
	k, err := scanA2AReceiverKey(repo.r.QueryRowContext(ctx,
		`SELECT `+a2aReceiverCols+` FROM a2a_receiver_keys WHERE secret_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.A2AReceiverKey{}, ErrNotFound
	}
	return k, err
}

func (repo *a2aReceiverKeyRepo) Revoke(ctx context.Context, scope domain.Scope, id, at string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE a2a_receiver_keys SET revoked_at = ?
		 WHERE id = ? AND org_id = ? AND environment_id = ? AND revoked_at = ''`,
		at, id, scope.OrgID, scope.EnvID,
	)
	return affectedOne(res, err)
}

func (repo *a2aReceiverKeyRepo) Touch(ctx context.Context, id, at string) error {
	_, err := repo.w.ExecContext(ctx, `UPDATE a2a_receiver_keys SET last_used_at = ? WHERE id = ?`, at, id)
	return err
}
