package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// IntegrationRepo persists provider credentials per (environment, channel).
// Every method is Scope-first; the unique key is (environment_id, identifier).
type IntegrationRepo interface {
	Create(ctx context.Context, scope domain.Scope, in domain.Integration) (domain.Integration, error)
	// GetActiveByChannel returns the active integration for the channel: prefer
	// primary_provider=1, else lowest priority value, deterministic tiebreak by id.
	GetActiveByChannel(ctx context.Context, scope domain.Scope, channel string) (domain.Integration, error)
}

type integrationRepo struct {
	w *sql.DB
	r *sql.DB
}

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
