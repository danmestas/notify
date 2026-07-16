package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// StreamSourceRepo persists inbound webhook-push sources per environment. Every
// method is Scope-first EXCEPT GetBySecretHash: the secret hash is the
// credential, so that lookup returns the owning row (and thus its scope) rather
// than being checked against a caller-supplied scope — the single, documented
// unscoped read, mirroring ApiKeys().GetByHash used by identity.Authenticate.
type StreamSourceRepo interface {
	Create(ctx context.Context, scope domain.Scope, src domain.StreamSource) (domain.StreamSource, error)
	// List returns the scope's sources ordered by created_at then id.
	// enc_secret is never selected; EncSecret is nil on every returned row.
	List(ctx context.Context, scope domain.Scope) ([]domain.StreamSource, error)
	// GetByID fetches one source by id, scope-checked. ErrNotFound when the id
	// is absent or belongs to another scope (same response, no info leak).
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.StreamSource, error)
	// GetBySecretHash resolves the source whose secret_hash matches. Unscoped:
	// the hash is the credential and identifies its own scope. ErrNotFound on a
	// miss.
	GetBySecretHash(ctx context.Context, hash string) (domain.StreamSource, error)
	// Update overwrites the mutable business fields (name, topic, the four
	// limits, active). It never touches id/org/environment, secret_hash,
	// enc_secret, created_at, or the activity columns.
	Update(ctx context.Context, scope domain.Scope, src domain.StreamSource) error
	Delete(ctx context.Context, scope domain.Scope, id string) error
	SetActive(ctx context.Context, scope domain.Scope, id string, active bool) error
	// TouchActivity updates only last_event_at + last_error, keyed by id alone
	// (best-effort hot-path write, no scope round-trip).
	TouchActivity(ctx context.Context, id, lastEventAt, lastError string) error
}

type streamSourceRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *streamSourceRepo) Create(ctx context.Context, scope domain.Scope, src domain.StreamSource) (domain.StreamSource, error) {
	src.OrgID = scope.OrgID
	src.EnvID = scope.EnvID
	if src.ID == "" {
		src.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO stream_sources
		   (id, org_id, environment_id, name, topic, secret_hash, enc_secret,
		    rate_limit_per_min, max_event_bytes, max_batch_events, byte_budget,
		    active, last_event_at, last_error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src.ID, src.OrgID, src.EnvID, src.Name, src.Topic, src.SecretHash, src.EncSecret,
		src.RateLimitPerMin, src.MaxEventBytes, src.MaxBatchEvents, src.ByteBudget,
		src.Active, src.LastEventAt, src.LastError, src.CreatedAt,
	)
	if err != nil {
		return domain.StreamSource{}, err
	}
	return src, nil
}

// List omits enc_secret so ciphertext never enters caller memory on the list
// path (same discipline as IntegrationRepo.List).
func (repo *streamSourceRepo) List(ctx context.Context, scope domain.Scope) ([]domain.StreamSource, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, name, topic, secret_hash,
		        rate_limit_per_min, max_event_bytes, max_batch_events, byte_budget,
		        active, last_event_at, last_error, created_at
		 FROM stream_sources
		 WHERE org_id = ? AND environment_id = ?
		 ORDER BY created_at ASC, id ASC`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.StreamSource
	for rows.Next() {
		var src domain.StreamSource
		if err := rows.Scan(
			&src.ID, &src.OrgID, &src.EnvID, &src.Name, &src.Topic, &src.SecretHash,
			&src.RateLimitPerMin, &src.MaxEventBytes, &src.MaxBatchEvents, &src.ByteBudget,
			&src.Active, &src.LastEventAt, &src.LastError, &src.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (repo *streamSourceRepo) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.StreamSource, error) {
	return repo.scanOne(
		ctx,
		`SELECT id, org_id, environment_id, name, topic, secret_hash, enc_secret,
		        rate_limit_per_min, max_event_bytes, max_batch_events, byte_budget,
		        active, last_event_at, last_error, created_at
		 FROM stream_sources
		 WHERE id = ? AND org_id = ? AND environment_id = ?`,
		id, scope.OrgID, scope.EnvID,
	)
}

func (repo *streamSourceRepo) GetBySecretHash(ctx context.Context, hash string) (domain.StreamSource, error) {
	return repo.scanOne(
		ctx,
		`SELECT id, org_id, environment_id, name, topic, secret_hash, enc_secret,
		        rate_limit_per_min, max_event_bytes, max_batch_events, byte_budget,
		        active, last_event_at, last_error, created_at
		 FROM stream_sources
		 WHERE secret_hash = ?`,
		hash,
	)
}

// scanOne runs a single-row SELECT that projects every column (enc_secret
// included) in canonical order and maps sql.ErrNoRows to ErrNotFound.
func (repo *streamSourceRepo) scanOne(ctx context.Context, query string, args ...any) (domain.StreamSource, error) {
	var src domain.StreamSource
	err := repo.r.QueryRowContext(ctx, query, args...).Scan(
		&src.ID, &src.OrgID, &src.EnvID, &src.Name, &src.Topic, &src.SecretHash, &src.EncSecret,
		&src.RateLimitPerMin, &src.MaxEventBytes, &src.MaxBatchEvents, &src.ByteBudget,
		&src.Active, &src.LastEventAt, &src.LastError, &src.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StreamSource{}, ErrNotFound
	}
	if err != nil {
		return domain.StreamSource{}, err
	}
	return src, nil
}

func (repo *streamSourceRepo) Update(ctx context.Context, scope domain.Scope, src domain.StreamSource) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE stream_sources
		 SET name = ?, topic = ?, rate_limit_per_min = ?, max_event_bytes = ?,
		     max_batch_events = ?, byte_budget = ?, active = ?
		 WHERE id = ? AND org_id = ? AND environment_id = ?`,
		src.Name, src.Topic, src.RateLimitPerMin, src.MaxEventBytes,
		src.MaxBatchEvents, src.ByteBudget, src.Active,
		src.ID, scope.OrgID, scope.EnvID,
	)
	return affectedOne(res, err)
}

func (repo *streamSourceRepo) Delete(ctx context.Context, scope domain.Scope, id string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM stream_sources WHERE id = ? AND org_id = ? AND environment_id = ?`,
		id, scope.OrgID, scope.EnvID,
	)
	return affectedOne(res, err)
}

func (repo *streamSourceRepo) SetActive(ctx context.Context, scope domain.Scope, id string, active bool) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE stream_sources SET active = ? WHERE id = ? AND org_id = ? AND environment_id = ?`,
		active, id, scope.OrgID, scope.EnvID,
	)
	return affectedOne(res, err)
}

// TouchActivity writes only the two activity columns, keyed by id. It is a
// best-effort hot-path write (the ingest handler ignores its error), so it does
// not carry a scope: the id already came from an authenticated source row.
func (repo *streamSourceRepo) TouchActivity(ctx context.Context, id, lastEventAt, lastError string) error {
	_, err := repo.w.ExecContext(
		ctx,
		`UPDATE stream_sources SET last_event_at = ?, last_error = ? WHERE id = ?`,
		lastEventAt, lastError, id,
	)
	return err
}

// affectedOne turns a write result into ErrNotFound when no row matched.
func affectedOne(res sql.Result, err error) error {
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
