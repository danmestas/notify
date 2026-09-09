package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// WebhookEndpointRepo persists outbound delivery-status callback URLs per
// environment. Every method is Scope-first.
type WebhookEndpointRepo interface {
	// Create inserts the endpoint, minting an id when empty.
	Create(ctx context.Context, scope domain.Scope, e domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
	// ListActiveByEnv returns the active endpoints for the scope, ordered by id.
	ListActiveByEnv(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error)
	// List returns all endpoints (active and inactive) ordered by id ASC.
	List(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error)
	// GetByID returns a single endpoint by its internal ULID id.
	// Returns ErrNotFound when no row matches.
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.WebhookEndpoint, error)
	// UpdateURL replaces the URL of an existing endpoint and returns the updated row.
	// Returns ErrNotFound when no row matches.
	UpdateURL(ctx context.Context, scope domain.Scope, id, url string) (domain.WebhookEndpoint, error)
	// SetActive enables or disables an endpoint and returns the updated row.
	// Returns ErrNotFound when no row matches.
	SetActive(ctx context.Context, scope domain.Scope, id string, active bool) (domain.WebhookEndpoint, error)
	// SetSecret stores the SEALED signing secret; "" clears it.
	SetSecret(ctx context.Context, scope domain.Scope, id, sealed string) (domain.WebhookEndpoint, error)
	// Delete permanently removes an endpoint.
	// Returns ErrNotFound when no row matches.
	Delete(ctx context.Context, scope domain.Scope, id string) error
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
		`INSERT INTO webhook_endpoints (id, org_id, environment_id, url, active, secret)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.OrgID, e.EnvID, e.URL, e.Active, e.Secret,
	)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	return e, nil
}

func (repo *webhookEndpointRepo) ListActiveByEnv(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, url, active, secret
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
		if err := rows.Scan(&e.ID, &e.OrgID, &e.EnvID, &e.URL, &e.Active, &e.Secret); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// List returns all endpoints (active and inactive) for the scope, ordered by id ASC.
func (repo *webhookEndpointRepo) List(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, url, active, secret
		 FROM webhook_endpoints
		 WHERE org_id = ? AND environment_id = ?
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
		if err := rows.Scan(&e.ID, &e.OrgID, &e.EnvID, &e.URL, &e.Active, &e.Secret); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetByID returns a single endpoint by its internal ULID id.
func (repo *webhookEndpointRepo) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.WebhookEndpoint, error) {
	var e domain.WebhookEndpoint
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, url, active, secret
		 FROM webhook_endpoints
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&e.ID, &e.OrgID, &e.EnvID, &e.URL, &e.Active, &e.Secret)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebhookEndpoint{}, ErrNotFound
	}
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	return e, nil
}

// UpdateURL replaces the URL of an existing endpoint and returns the updated row.
func (repo *webhookEndpointRepo) UpdateURL(ctx context.Context, scope domain.Scope, id, url string) (domain.WebhookEndpoint, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE webhook_endpoints SET url = ?
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		url, scope.OrgID, scope.EnvID, id,
	)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if n == 0 {
		return domain.WebhookEndpoint{}, ErrNotFound
	}
	return repo.GetByID(ctx, scope, id)
}

// SetActive enables or disables an endpoint and returns the updated row.
// SetSecret stores a sealed signing secret for the endpoint and returns the
// updated row. The caller seals; this repo never sees plaintext.
func (repo *webhookEndpointRepo) SetSecret(ctx context.Context, scope domain.Scope, id, sealed string) (domain.WebhookEndpoint, error) {
	res, err := repo.w.ExecContext(ctx,
		`UPDATE webhook_endpoints SET secret = ? WHERE id = ? AND org_id = ? AND environment_id = ?`,
		sealed, id, scope.OrgID, scope.EnvID)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.WebhookEndpoint{}, ErrNotFound
	}
	return repo.GetByID(ctx, scope, id)
}

func (repo *webhookEndpointRepo) SetActive(ctx context.Context, scope domain.Scope, id string, active bool) (domain.WebhookEndpoint, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE webhook_endpoints SET active = ?
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		active, scope.OrgID, scope.EnvID, id,
	)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if n == 0 {
		return domain.WebhookEndpoint{}, ErrNotFound
	}
	return repo.GetByID(ctx, scope, id)
}

// Delete permanently removes an endpoint. Returns ErrNotFound when no row
// matches the scope and id.
func (repo *webhookEndpointRepo) Delete(ctx context.Context, scope domain.Scope, id string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM webhook_endpoints
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
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
