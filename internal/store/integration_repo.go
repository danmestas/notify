package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/danmestas/notify/internal/domain"
)

// IntegrationRepo persists provider credentials per (environment, channel).
// Every method is Scope-first; the unique key is (environment_id, identifier).
type IntegrationRepo interface {
	Create(ctx context.Context, scope domain.Scope, in domain.Integration) (domain.Integration, error)
	// GetActiveByChannel returns the active integration for the channel: prefer
	// primary_provider=1, else lowest priority value, deterministic tiebreak by id.
	GetActiveByChannel(ctx context.Context, scope domain.Scope, channel string) (domain.Integration, error)
	// List returns all integrations for the scope ordered by channel, primary
	// (desc), priority (asc), id (asc). enc_credentials is never selected;
	// EncCredentials on each returned Integration is nil.
	List(ctx context.Context, scope domain.Scope) ([]domain.Integration, error)
	// GetByID fetches a single integration by ID, scope-checked. Returns
	// ErrNotFound when the id does not exist or belongs to a different scope
	// (both cases produce the same response to avoid information leakage).
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Integration, error)
	// GetByProviderID fetches the scope's integration for providerID
	// regardless of the active flag or channel priority ordering. Unlike
	// GetActiveByChannel (which resolves the one provider a channel actually
	// dispatches through), this is a direct singleton lookup — used for
	// provider rows, such as the auto-minted webpush VAPID keypair, that are
	// addressed by their known provider id rather than selected by channel.
	GetByProviderID(ctx context.Context, scope domain.Scope, providerID string) (domain.Integration, error)
	// Update overwrites the mutable fields (identifier, enc_credentials,
	// primary_provider, priority, active) of an existing integration. It must
	// not touch provider_id, channel, org_id, or environment_id.
	Update(ctx context.Context, scope domain.Scope, in domain.Integration) error
	// Delete removes the integration permanently. Returns ErrNotFound when the
	// row does not exist in this scope.
	Delete(ctx context.Context, scope domain.Scope, id string) error
	// SetActive atomically flips the active flag on a single integration row.
	SetActive(ctx context.Context, scope domain.Scope, id string, active bool) error
}

type integrationRepo struct {
	w *sql.DB
	r *sql.DB
}

// Create inserts a new integration row. A duplicate (environment_id,
// identifier) — e.g. two concurrent first-use mints racing to create the
// same singleton provider row — maps to ErrConflict; Create never updates an
// existing row.
func (repo *integrationRepo) Create(ctx context.Context, scope domain.Scope, in domain.Integration) (domain.Integration, error) {
	in.OrgID = scope.OrgID
	in.EnvID = scope.EnvID
	if in.ID == "" {
		in.ID = newULID()
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO integrations
		   (id, org_id, environment_id, provider_id, channel, enc_credentials,
		    configurations, active, primary_provider, priority, identifier)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ID, in.OrgID, in.EnvID, in.ProviderID, in.Channel, in.EncCredentials,
		[]byte(in.Configurations), in.Active, in.Primary, in.Priority, in.Identifier,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return domain.Integration{}, ErrConflict
		}
		return domain.Integration{}, err
	}
	return in, nil
}

func (repo *integrationRepo) GetActiveByChannel(ctx context.Context, scope domain.Scope, channel string) (domain.Integration, error) {
	var (
		in  domain.Integration
		cfg []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, provider_id, channel, enc_credentials,
		        configurations, active, primary_provider, priority, identifier
		 FROM integrations
		 WHERE org_id = ? AND environment_id = ? AND channel = ? AND active = 1
		 ORDER BY primary_provider DESC, priority ASC, id ASC
		 LIMIT 1`,
		scope.OrgID, scope.EnvID, channel,
	).Scan(&in.ID, &in.OrgID, &in.EnvID, &in.ProviderID, &in.Channel, &in.EncCredentials,
		&cfg, &in.Active, &in.Primary, &in.Priority, &in.Identifier)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Integration{}, ErrNotFound
	}
	if err != nil {
		return domain.Integration{}, err
	}
	in.Configurations = cfg
	return in, nil
}

// GetByProviderID fetches the scope's single integration row for providerID,
// ignoring the active flag. It scans the same columns as GetByID (including
// enc_credentials) but filters on provider_id instead of id.
func (repo *integrationRepo) GetByProviderID(ctx context.Context, scope domain.Scope, providerID string) (domain.Integration, error) {
	var (
		in  domain.Integration
		cfg []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, provider_id, channel, enc_credentials,
		        configurations, active, primary_provider, priority, identifier
		 FROM integrations
		 WHERE org_id = ? AND environment_id = ? AND provider_id = ?
		 ORDER BY id ASC
		 LIMIT 1`,
		scope.OrgID, scope.EnvID, providerID,
	).Scan(&in.ID, &in.OrgID, &in.EnvID, &in.ProviderID, &in.Channel, &in.EncCredentials,
		&cfg, &in.Active, &in.Primary, &in.Priority, &in.Identifier)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Integration{}, ErrNotFound
	}
	if err != nil {
		return domain.Integration{}, err
	}
	in.Configurations = cfg
	return in, nil
}

// List returns all integrations for the scope. enc_credentials is deliberately
// omitted from the SELECT so that the returned slice never carries ciphertext
// into caller memory or JSON serialisation.
func (repo *integrationRepo) List(ctx context.Context, scope domain.Scope) ([]domain.Integration, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, provider_id, channel,
		        configurations, active, primary_provider, priority, identifier
		 FROM integrations
		 WHERE org_id = ? AND environment_id = ?
		 ORDER BY channel ASC, primary_provider DESC, priority ASC, id ASC`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Integration
	for rows.Next() {
		var (
			in  domain.Integration
			cfg []byte
		)
		if err := rows.Scan(
			&in.ID, &in.OrgID, &in.EnvID, &in.ProviderID, &in.Channel,
			&cfg, &in.Active, &in.Primary, &in.Priority, &in.Identifier,
		); err != nil {
			return nil, err
		}
		in.Configurations = cfg
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetByID fetches a single integration by ID with scope-check. The query
// includes enc_credentials so the connectivity layer can decrypt on demand.
func (repo *integrationRepo) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Integration, error) {
	var (
		in  domain.Integration
		cfg []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, provider_id, channel, enc_credentials,
		        configurations, active, primary_provider, priority, identifier
		 FROM integrations
		 WHERE id = ? AND org_id = ? AND environment_id = ?`,
		id, scope.OrgID, scope.EnvID,
	).Scan(&in.ID, &in.OrgID, &in.EnvID, &in.ProviderID, &in.Channel, &in.EncCredentials,
		&cfg, &in.Active, &in.Primary, &in.Priority, &in.Identifier)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Integration{}, ErrNotFound
	}
	if err != nil {
		return domain.Integration{}, err
	}
	in.Configurations = cfg
	return in, nil
}

// Update overwrites the mutable columns of an existing integration. provider_id,
// channel, org_id, and environment_id are immutable and are not touched.
func (repo *integrationRepo) Update(ctx context.Context, scope domain.Scope, in domain.Integration) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE integrations
		 SET identifier = ?, enc_credentials = ?, configurations = ?,
		     active = ?, primary_provider = ?, priority = ?
		 WHERE id = ? AND org_id = ? AND environment_id = ?`,
		in.Identifier, in.EncCredentials, []byte(in.Configurations),
		in.Active, in.Primary, in.Priority,
		in.ID, scope.OrgID, scope.EnvID,
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

// Delete removes the integration row. Returns ErrNotFound when the row is
// absent or belongs to a different scope.
func (repo *integrationRepo) Delete(ctx context.Context, scope domain.Scope, id string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM integrations WHERE id = ? AND org_id = ? AND environment_id = ?`,
		id, scope.OrgID, scope.EnvID,
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

// SetActive atomically flips the active flag. Returns ErrNotFound when the row
// is absent or belongs to a different scope.
func (repo *integrationRepo) SetActive(ctx context.Context, scope domain.Scope, id string, active bool) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE integrations SET active = ? WHERE id = ? AND org_id = ? AND environment_id = ?`,
		active, id, scope.OrgID, scope.EnvID,
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
