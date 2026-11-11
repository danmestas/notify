package store

import (
	"context"
	"database/sql"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// OrganizationRepo is a tenancy root: Create mints an org id rather than
// requiring a Scope.
type OrganizationRepo interface {
	Create(ctx context.Context, name string) (domain.Organization, error)
	// FindByName returns every organization with exactly this name, oldest
	// first. Names are not unique, so callers must handle more than one.
	FindByName(ctx context.Context, name string) ([]domain.Organization, error)
}

type orgRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *orgRepo) Create(ctx context.Context, name string) (domain.Organization, error) {
	org := domain.Organization{
		ID:        newULID(),
		Name:      name,
		CreatedAt: timestamp.Now(),
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)`,
		org.ID, org.Name, org.CreatedAt,
	)
	if err != nil {
		return domain.Organization{}, err
	}
	return org, nil
}

func (repo *orgRepo) FindByName(ctx context.Context, name string) ([]domain.Organization, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, name, created_at FROM organizations WHERE name = ? ORDER BY created_at, id`,
		name,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var orgs []domain.Organization
	for rows.Next() {
		var org domain.Organization
		if err := rows.Scan(&org.ID, &org.Name, &org.CreatedAt); err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, rows.Err()
}
