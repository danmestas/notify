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
