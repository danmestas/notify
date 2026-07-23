package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// ErrContextConflict is returned by Create when the (org, env, type, external_id)
// composite key already exists.
var ErrContextConflict = fmt.Errorf("context with this type and id already exists")

// ContextRepo persists reusable JSON data objects keyed by (type, external_id)
// within a Scope. Every method is Scope-first; cross-env isolation is enforced
// by always binding org_id + environment_id in every query.
type ContextRepo interface {
	// Create inserts a new context. Returns ErrContextConflict on duplicate (org,env,type,external_id).
	Create(ctx context.Context, scope domain.Scope, ctype, externalID string, data json.RawMessage) (domain.Context, error)
	// Get returns a single context by composite natural key. Returns ErrNotFound when absent.
	Get(ctx context.Context, scope domain.Scope, ctype, externalID string) (domain.Context, error)
	// GetByID returns a single context by internal ULID id. Returns ErrNotFound when absent.
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Context, error)
	// List returns contexts in scope, keyset-paginated by id ASC.
	// search filters by type prefix (case-insensitive). after/before are opaque ULID cursors.
	List(ctx context.Context, scope domain.Scope, ctype, search, after, before string, limit int) (items []domain.Context, nextCursor, prevCursor string, err error)
	// Update replaces the data field. Returns ErrNotFound when absent.
	Update(ctx context.Context, scope domain.Scope, ctype, externalID string, data json.RawMessage) (domain.Context, error)
	// Delete removes a context permanently. Returns ErrNotFound when absent.
	Delete(ctx context.Context, scope domain.Scope, ctype, externalID string) error
}

type contextRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *contextRepo) Create(ctx context.Context, scope domain.Scope, ctype, externalID string, data json.RawMessage) (domain.Context, error) {
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	id := newULID()
	now := timestamp.Now()
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO contexts (id, org_id, environment_id, type, external_id, data, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, scope.OrgID, scope.EnvID, ctype, externalID, string(data), now, now,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return domain.Context{}, ErrContextConflict
		}
		return domain.Context{}, err
	}
	return domain.Context{
		ID: id, OrgID: scope.OrgID, EnvID: scope.EnvID,
		Type: ctype, ExternalID: externalID, Data: data,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (repo *contextRepo) Get(ctx context.Context, scope domain.Scope, ctype, externalID string) (domain.Context, error) {
	var c domain.Context
	var data []byte
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, type, external_id, data, created_at, updated_at
		 FROM contexts
		 WHERE org_id = ? AND environment_id = ? AND type = ? AND external_id = ?`,
		scope.OrgID, scope.EnvID, ctype, externalID,
	).Scan(&c.ID, &c.OrgID, &c.EnvID, &c.Type, &c.ExternalID, &data, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Context{}, ErrNotFound
	}
	if err != nil {
		return domain.Context{}, err
	}
	c.Data = data
	return c, nil
}

func (repo *contextRepo) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Context, error) {
	var c domain.Context
	var data []byte
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, type, external_id, data, created_at, updated_at
		 FROM contexts WHERE id = ? AND org_id = ? AND environment_id = ?`,
		id, scope.OrgID, scope.EnvID,
	).Scan(&c.ID, &c.OrgID, &c.EnvID, &c.Type, &c.ExternalID, &data, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Context{}, ErrNotFound
	}
	if err != nil {
		return domain.Context{}, err
	}
	c.Data = data
	return c, nil
}

func (repo *contextRepo) List(ctx context.Context, scope domain.Scope, ctype, search, after, before string, limit int) ([]domain.Context, string, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	// Build WHERE clause.
	args := []any{scope.OrgID, scope.EnvID}
	conds := []string{"org_id = ?", "environment_id = ?"}
	if ctype != "" {
		conds = append(conds, "type = ?")
		args = append(args, ctype)
	}
	if search != "" {
		conds = append(conds, "(type LIKE ? OR external_id LIKE ?)")
		pat := "%" + strings.ToLower(search) + "%"
		args = append(args, pat, pat)
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
		`SELECT id, org_id, environment_id, type, external_id, data, created_at, updated_at
		 FROM contexts WHERE %s ORDER BY id %s LIMIT ?`,
		where, dir,
	)
	args = append(args, limit+1)

	rows, err := repo.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.Context
	for rows.Next() {
		var c domain.Context
		var data []byte
		if err := rows.Scan(&c.ID, &c.OrgID, &c.EnvID, &c.Type, &c.ExternalID, &data, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, "", "", err
		}
		c.Data = data
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", "", err
	}

	if before != "" {
		// Reverse because we queried DESC.
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

func (repo *contextRepo) Update(ctx context.Context, scope domain.Scope, ctype, externalID string, data json.RawMessage) (domain.Context, error) {
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	now := timestamp.Now()
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE contexts SET data = ?, updated_at = ?
		 WHERE org_id = ? AND environment_id = ? AND type = ? AND external_id = ?`,
		string(data), now, scope.OrgID, scope.EnvID, ctype, externalID,
	)
	if err != nil {
		return domain.Context{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.Context{}, ErrNotFound
	}
	return repo.Get(ctx, scope, ctype, externalID)
}

func (repo *contextRepo) Delete(ctx context.Context, scope domain.Scope, ctype, externalID string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM contexts WHERE org_id = ? AND environment_id = ? AND type = ? AND external_id = ?`,
		scope.OrgID, scope.EnvID, ctype, externalID,
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

// isUniqueConstraintError detects SQLite UNIQUE constraint violations.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
