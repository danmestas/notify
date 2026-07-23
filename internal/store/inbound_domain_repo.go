package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// ErrInboundDomainConflict is returned by Create when (org,env,name) already exists.
var ErrInboundDomainConflict = fmt.Errorf("inbound domain already exists")

// InboundDomainRepo persists inbound domains scoped to (org_id, environment_id).
type InboundDomainRepo interface {
	Create(ctx context.Context, scope domain.Scope, name, expectedMXHost string, expectedMXPriority int) (domain.InboundDomain, error)
	Get(ctx context.Context, scope domain.Scope, name string) (domain.InboundDomain, error)
	List(ctx context.Context, scope domain.Scope, search, after string, limit int) (items []domain.InboundDomain, nextCursor string, total int, err error)
	SetStatus(ctx context.Context, scope domain.Scope, name, status string, mxConfigured bool) error
	Delete(ctx context.Context, scope domain.Scope, name string) error
}

type inboundDomainRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *inboundDomainRepo) Create(ctx context.Context, scope domain.Scope, name, expectedMXHost string, expectedMXPriority int) (domain.InboundDomain, error) {
	id := newULID()
	now := timestamp.Now()
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO inbound_domains (id, org_id, environment_id, name, status, mx_configured, expected_mx_host, expected_mx_priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?)`,
		id, scope.OrgID, scope.EnvID, name, expectedMXHost, expectedMXPriority, now, now,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return domain.InboundDomain{}, ErrInboundDomainConflict
		}
		return domain.InboundDomain{}, err
	}
	return domain.InboundDomain{
		ID: id, OrgID: scope.OrgID, EnvID: scope.EnvID,
		Name: name, Status: "pending", MXConfigured: false,
		ExpectedMXHost: expectedMXHost, ExpectedMXPriority: expectedMXPriority,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (repo *inboundDomainRepo) Get(ctx context.Context, scope domain.Scope, name string) (domain.InboundDomain, error) {
	var d domain.InboundDomain
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, name, status, mx_configured, expected_mx_host, expected_mx_priority, created_at, updated_at
		 FROM inbound_domains
		 WHERE org_id = ? AND environment_id = ? AND name = ?`,
		scope.OrgID, scope.EnvID, name,
	).Scan(&d.ID, &d.OrgID, &d.EnvID, &d.Name, &d.Status, &d.MXConfigured,
		&d.ExpectedMXHost, &d.ExpectedMXPriority, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InboundDomain{}, ErrNotFound
	}
	return d, err
}

func (repo *inboundDomainRepo) List(ctx context.Context, scope domain.Scope, search, after string, limit int) ([]domain.InboundDomain, string, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var total int
	if err := repo.r.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM inbound_domains WHERE org_id = ? AND environment_id = ? AND (? = '' OR name LIKE '%' || ? || '%')`,
		scope.OrgID, scope.EnvID, search, search,
	).Scan(&total); err != nil {
		return nil, "", 0, err
	}

	var rows *sql.Rows
	var err error
	if after == "" {
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT id, org_id, environment_id, name, status, mx_configured, expected_mx_host, expected_mx_priority, created_at, updated_at
			 FROM inbound_domains
			 WHERE org_id = ? AND environment_id = ? AND (? = '' OR name LIKE '%' || ? || '%')
			 ORDER BY name ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, search, search, limit+1,
		)
	} else {
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT id, org_id, environment_id, name, status, mx_configured, expected_mx_host, expected_mx_priority, created_at, updated_at
			 FROM inbound_domains
			 WHERE org_id = ? AND environment_id = ? AND (? = '' OR name LIKE '%' || ? || '%') AND name > ?
			 ORDER BY name ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, search, search, after, limit+1,
		)
	}
	if err != nil {
		return nil, "", 0, err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.InboundDomain
	for rows.Next() {
		var d domain.InboundDomain
		if err := rows.Scan(&d.ID, &d.OrgID, &d.EnvID, &d.Name, &d.Status, &d.MXConfigured,
			&d.ExpectedMXHost, &d.ExpectedMXPriority, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, "", 0, err
		}
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, err
	}

	var nextCursor string
	if len(items) > limit {
		items = items[:limit]
		// The cursor is the last returned row's name; the next page's
		// WHERE clause filters on name > cursor, so a row is never skipped.
		nextCursor = items[limit-1].Name
	}
	return items, nextCursor, total, nil
}

func (repo *inboundDomainRepo) SetStatus(ctx context.Context, scope domain.Scope, name, status string, mxConfigured bool) error {
	now := timestamp.Now()
	mx := 0
	if mxConfigured {
		mx = 1
	}
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE inbound_domains SET status = ?, mx_configured = ?, updated_at = ?
		 WHERE org_id = ? AND environment_id = ? AND name = ?`,
		status, mx, now, scope.OrgID, scope.EnvID, name,
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

func (repo *inboundDomainRepo) Delete(ctx context.Context, scope domain.Scope, name string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM inbound_domains WHERE org_id = ? AND environment_id = ? AND name = ?`,
		scope.OrgID, scope.EnvID, name,
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
