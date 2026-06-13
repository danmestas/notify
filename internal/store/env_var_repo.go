package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// EnvVarRepo persists AES-GCM-encrypted environment variables keyed by
// (org_id, environment_id, key). Every method is Scope-first; cross-env
// isolation is enforced by always binding org_id + environment_id.
type EnvVarRepo interface {
	// Upsert inserts or replaces the env var for (scope, key). valueEnc is the
	// already-encrypted blob. Returns the upserted row (without decrypted value).
	Upsert(ctx context.Context, scope domain.Scope, key string, isSecret bool, valueEnc []byte) (domain.EnvVar, error)
	// Get returns the env var row for (scope, key). Returns ErrNotFound when absent.
	Get(ctx context.Context, scope domain.Scope, key string) (domain.EnvVar, error)
	// GetValue returns the encrypted value blob for (scope, key). Returns ErrNotFound when absent.
	GetValue(ctx context.Context, scope domain.Scope, key string) ([]byte, error)
	// List returns all env vars for scope, optionally filtered by key LIKE %search%.
	// Keyset-paginated by id ASC: after/before are opaque ULID cursors.
	List(ctx context.Context, scope domain.Scope, search, after, before string, limit int) (items []domain.EnvVar, nextCursor, prevCursor string, err error)
	// Delete removes the env var and its value. Returns ErrNotFound when absent.
	Delete(ctx context.Context, scope domain.Scope, key string) error
}

type envVarRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *envVarRepo) Upsert(ctx context.Context, scope domain.Scope, key string, isSecret bool, valueEnc []byte) (domain.EnvVar, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	isSecretInt := 0
	if isSecret {
		isSecretInt = 1
	}

	// Use a transaction so env_vars + env_var_values stay in sync.
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return domain.EnvVar{}, fmt.Errorf("envVarRepo.Upsert: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Check whether a row already exists for this (org, env, key).
	var existingID string
	var existingCreatedAt string
	lookupErr := tx.QueryRowContext(
		ctx,
		`SELECT id, created_at FROM env_vars WHERE org_id = ? AND environment_id = ? AND key = ?`,
		scope.OrgID, scope.EnvID, key,
	).Scan(&existingID, &existingCreatedAt)

	var id string
	var createdAt string
	if errors.Is(lookupErr, sql.ErrNoRows) {
		// Insert new row.
		id = newULID()
		createdAt = now
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO env_vars (id, org_id, environment_id, key, is_secret, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, scope.OrgID, scope.EnvID, key, isSecretInt, createdAt, now,
		)
		if err != nil {
			return domain.EnvVar{}, fmt.Errorf("envVarRepo.Upsert: insert env_var: %w", err)
		}
	} else if lookupErr != nil {
		return domain.EnvVar{}, fmt.Errorf("envVarRepo.Upsert: lookup: %w", lookupErr)
	} else {
		// Update existing row.
		id = existingID
		createdAt = existingCreatedAt
		_, err = tx.ExecContext(
			ctx,
			`UPDATE env_vars SET is_secret = ?, updated_at = ? WHERE id = ?`,
			isSecretInt, now, id,
		)
		if err != nil {
			return domain.EnvVar{}, fmt.Errorf("envVarRepo.Upsert: update env_var: %w", err)
		}
	}

	// Upsert the value blob.
	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO env_var_values (env_var_id, environment_id, value_enc)
		 VALUES (?, ?, ?)
		 ON CONFLICT(env_var_id, environment_id) DO UPDATE SET value_enc = excluded.value_enc`,
		id, scope.EnvID, valueEnc,
	)
	if err != nil {
		return domain.EnvVar{}, fmt.Errorf("envVarRepo.Upsert: upsert value: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.EnvVar{}, fmt.Errorf("envVarRepo.Upsert: commit: %w", err)
	}

	return domain.EnvVar{
		ID:        id,
		OrgID:     scope.OrgID,
		EnvID:     scope.EnvID,
		Key:       key,
		IsSecret:  isSecret,
		CreatedAt: createdAt,
		UpdatedAt: now,
	}, nil
}

func (repo *envVarRepo) Get(ctx context.Context, scope domain.Scope, key string) (domain.EnvVar, error) {
	var ev domain.EnvVar
	var isSecretInt int
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, key, is_secret, created_at, updated_at
		 FROM env_vars
		 WHERE org_id = ? AND environment_id = ? AND key = ?`,
		scope.OrgID, scope.EnvID, key,
	).Scan(&ev.ID, &ev.OrgID, &ev.EnvID, &ev.Key, &isSecretInt, &ev.CreatedAt, &ev.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EnvVar{}, ErrNotFound
	}
	if err != nil {
		return domain.EnvVar{}, err
	}
	ev.IsSecret = isSecretInt != 0
	return ev, nil
}

func (repo *envVarRepo) GetValue(ctx context.Context, scope domain.Scope, key string) ([]byte, error) {
	var valueEnc []byte
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT vv.value_enc
		 FROM env_var_values vv
		 JOIN env_vars ev ON ev.id = vv.env_var_id
		 WHERE ev.org_id = ? AND ev.environment_id = ? AND ev.key = ?`,
		scope.OrgID, scope.EnvID, key,
	).Scan(&valueEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return valueEnc, nil
}

func (repo *envVarRepo) List(ctx context.Context, scope domain.Scope, search, after, before string, limit int) ([]domain.EnvVar, string, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	args := []any{scope.OrgID, scope.EnvID}
	conds := []string{"org_id = ?", "environment_id = ?"}
	if search != "" {
		conds = append(conds, "key LIKE ?")
		args = append(args, "%"+strings.ToLower(search)+"%")
	}
	dir := "ASC"
	if before != "" {
		conds = append(conds, "id < ?")
		args = append(args, before)
		dir = "DESC"
	} else if after != "" {
		conds = append(conds, "id > ?")
		args = append(args, after)
	}
	where := strings.Join(conds, " AND ")
	q := fmt.Sprintf(
		`SELECT id, org_id, environment_id, key, is_secret, created_at, updated_at
		 FROM env_vars WHERE %s ORDER BY id %s LIMIT ?`,
		where, dir,
	)
	args = append(args, limit+1)

	rows, err := repo.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.EnvVar
	for rows.Next() {
		var ev domain.EnvVar
		var isSecretInt int
		if err := rows.Scan(&ev.ID, &ev.OrgID, &ev.EnvID, &ev.Key, &isSecretInt, &ev.CreatedAt, &ev.UpdatedAt); err != nil {
			return nil, "", "", err
		}
		ev.IsSecret = isSecretInt != 0
		items = append(items, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", "", err
	}

	if before != "" {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}

	var nextCursor, prevCursor string
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if len(items) > 0 {
		if hasMore || after != "" {
			nextCursor = items[len(items)-1].ID
		}
		if after != "" || (before != "" && hasMore) {
			prevCursor = items[0].ID
		}
	}
	return items, nextCursor, prevCursor, nil
}

func (repo *envVarRepo) Delete(ctx context.Context, scope domain.Scope, key string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM env_vars WHERE org_id = ? AND environment_id = ? AND key = ?`,
		scope.OrgID, scope.EnvID, key,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
