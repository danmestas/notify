package store

import (
	"context"
	"database/sql"

	"github.com/danmestas/notify/internal/domain"
)

// WebhookEndpointRepo persists outbound delivery-status callback URLs per
// environment. Every method is Scope-first.
type WebhookEndpointRepo interface {
	// Create inserts the endpoint, minting an id when empty.
	Create(ctx context.Context, scope domain.Scope, e domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
	// ListActiveByEnv returns the active endpoints for the scope, ordered by id.
	ListActiveByEnv(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error)
}

type webhookEndpointRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *webhookEndpointRepo) Create(ctx context.Context, scope domain.Scope, e domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	e.OrgID = scope.OrgID
	e.EnvID = scope.EnvID
	if e.ID == "" {
		e.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO webhook_endpoints (id, org_id, environment_id, url, active)
		 VALUES (?, ?, ?, ?, ?)`,
		e.ID, e.OrgID, e.EnvID, e.URL, e.Active,
	)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	return e, nil
}

func (repo *webhookEndpointRepo) ListActiveByEnv(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, url, active
		 FROM webhook_endpoints
		 WHERE org_id = ? AND environment_id = ? AND active = 1
		 ORDER BY id ASC`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.WebhookEndpoint
	for rows.Next() {
		var e domain.WebhookEndpoint
		if err := rows.Scan(&e.ID, &e.OrgID, &e.EnvID, &e.URL, &e.Active); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
