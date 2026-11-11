package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
)

var (
	// ErrTenantSpec marks a tenant spec missing its organization or
	// environment name.
	ErrTenantSpec = errors.New("identity: organization and environment are required")
	// ErrTenantConflict marks a spec that names more than one existing
	// organization, so no single tenant can be chosen for it.
	ErrTenantConflict = errors.New("identity: tenant spec is ambiguous")
)

// Tenant is the outcome of EnsureTenant.
type Tenant struct {
	Scope domain.Scope
	// APIKey is the plaintext key, set only when this call created the
	// environment. It is never recoverable afterwards except via rotation.
	APIKey  string
	Created bool
}

// EnsureTenant is the idempotent form of Bootstrap: it finds the environment
// named envIdentifier under the organization named orgName, creating the
// organization and/or environment (and its first API key) when absent. When
// everything already exists it returns the existing scope with Created false
// and no key. Two organizations sharing orgName yield ErrTenantConflict.
func (svc *Service) EnsureTenant(ctx context.Context, orgName, envIdentifier string) (Tenant, error) {
	if orgName == "" || envIdentifier == "" {
		return Tenant{}, ErrTenantSpec
	}
	svc.ensureMu.Lock()
	defer svc.ensureMu.Unlock()

	orgs, err := svc.store.Organizations().FindByName(ctx, orgName)
	if err != nil {
		return Tenant{}, fmt.Errorf("identity: find org: %w", err)
	}
	switch len(orgs) {
	case 0:
		return created(svc.Bootstrap(ctx, orgName, envIdentifier))
	case 1:
	default:
		return Tenant{}, fmt.Errorf("%w: %d organizations are named %q", ErrTenantConflict, len(orgs), orgName)
	}

	envs, err := svc.store.Environments().List(ctx, orgs[0].ID)
	if err != nil {
		return Tenant{}, fmt.Errorf("identity: list envs: %w", err)
	}
	for _, env := range envs {
		if env.Identifier == envIdentifier {
			return Tenant{Scope: domain.Scope{OrgID: env.OrgID, EnvID: env.ID}}, nil
		}
	}
	return created(svc.provisionEnv(ctx, orgs[0].ID, envIdentifier))
}

func created(key string, scope domain.Scope, err error) (Tenant, error) {
	if err != nil {
		return Tenant{}, err
	}
	return Tenant{Scope: scope, APIKey: key, Created: true}, nil
}
