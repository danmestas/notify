package notify

import (
	"context"
	"errors"

	"github.com/danmestas/notify/internal/identity"
)

// ErrTenantSpec is returned for a TenantSpec missing its Organization or
// Environment.
var ErrTenantSpec = identity.ErrTenantSpec

// ErrTenantConflict is returned when a TenantSpec names an organization that
// matches more than one existing organization, so no single tenant can be
// chosen. Resolve it by hand (notify rotate-key accepts ids).
var ErrTenantConflict = identity.ErrTenantConflict

// TenantSpec names a tenant: an organization and one environment of it.
type TenantSpec struct {
	// Organization is the organization's name.
	Organization string
	// Environment is the environment's identifier within the organization.
	Environment string
}

// Tenant is a tenant's identifiers and, when EnsureTenant just created it, its
// API key.
type Tenant struct {
	OrgID string
	EnvID string
	// APIKey is the plaintext API key, authenticating as "Authorization:
	// ApiKey <key>". It is set only when Created is true: notify stores it
	// hashed and sealed, so it cannot be shown again. Keep it or rotate.
	APIKey string
	// Created reports whether this call created anything. False means the
	// tenant already existed and APIKey is empty.
	Created bool
}

// EnsureTenant creates the tenant spec names, or finds it, and is safe to call
// on every start. It is the programmatic form of `notify bootstrap`.
//
//   - Neither organization nor environment exists: both are created, with an
//     API key.
//   - The organization exists without that environment: the environment is
//     created under it, with an API key.
//   - Both exist: their ids are returned with Created false and no key.
//   - Several organizations share the name: ErrTenantConflict.
//   - An empty Organization or Environment: ErrTenantSpec.
func (n *Notify) EnsureTenant(ctx context.Context, spec TenantSpec) (Tenant, error) {
	t, err := n.identity.EnsureTenant(ctx, spec.Organization, spec.Environment)
	if err != nil {
		return Tenant{}, err
	}
	return Tenant{
		OrgID: t.Scope.OrgID, EnvID: t.Scope.EnvID, APIKey: t.APIKey, Created: t.Created,
	}, nil
}

// EnsureTenant is Notify.EnsureTenant on the running notify. It errors before
// Start and after Stop.
func (p *Program) EnsureTenant(ctx context.Context, spec TenantSpec) (Tenant, error) {
	n := p.notify()
	if n == nil {
		return Tenant{}, errors.New("notify: EnsureTenant: notify is not running")
	}
	return n.EnsureTenant(ctx, spec)
}
