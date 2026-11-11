package notify

import (
	"context"
	"errors"

	"github.com/danmestas/notify/internal/identity"
)

// ErrNotRunning is returned by Program.EnsureTenant before Start or after
// Stop.
var ErrNotRunning = errors.New("notify: not running")

// ErrTenantSpec is returned for a TenantSpec whose Organization or Environment
// is empty once surrounding spaces are trimmed.
var ErrTenantSpec = identity.ErrTenantSpec

// ErrTenantConflict is returned when a TenantSpec names an organization that
// matches more than one existing organization, so no single tenant can be
// chosen. Resolve it by hand (notify rotate-key accepts ids).
var ErrTenantConflict = identity.ErrTenantConflict

// TenantSpec names a tenant: an organization and one environment of it.
type TenantSpec struct {
	// Organization is the organization's name.
	Organization string
	// Environment names the environment within the organization. It is
	// normalised like an environment created in the console: trimmed, lower
	// cased, spaces turned to hyphens ("Staging East" is "staging-east"), and
	// the trimmed text becomes its display name.
	Environment string
}

// Tenant is a tenant's identifiers and, when EnsureTenant just created it, its
// API key.
type Tenant struct {
	OrgID string
	EnvID string
	// APIKey is the plaintext API key, authenticating as "Authorization:
	// ApiKey <key>". It is set only when KeyIssued is true: notify stores it
	// hashed and sealed, so it cannot be shown again. Keep it or rotate.
	APIKey string
	// KeyIssued reports that this call minted APIKey: the environment was
	// created, or it existed with no key at all (left by an interrupted older
	// bootstrap). False means the tenant already had a key and APIKey is empty.
	KeyIssued bool
}

// EnsureTenant creates the tenant spec names, or finds it, and is safe to call
// on every start. It is the programmatic form of `notify bootstrap`.
//
//   - Neither organization nor environment exists: both are created, with an
//     API key.
//   - The organization exists without that environment: the environment is
//     created under it, with an API key; other environments' keys stay valid.
//   - Both exist and the environment has a key: their ids are returned with
//     KeyIssued false and no key.
//   - Both exist but the environment has no key: one is issued.
//   - Several organizations share the name: ErrTenantConflict.
//   - An empty Organization or Environment: ErrTenantSpec.
//
// The whole call is one SQLite transaction that takes the write lock first, so
// it is atomic and safe across goroutines and across processes sharing the
// database file.
//
// EnsureTenant checks neither dev mode nor the master key: that is the
// caller's job (New already requires a valid master key).
func (n *Notify) EnsureTenant(ctx context.Context, spec TenantSpec) (Tenant, error) {
	t, err := n.identity.EnsureTenant(ctx, spec.Organization, spec.Environment)
	if err != nil {
		return Tenant{}, err
	}
	return Tenant{
		OrgID: t.Scope.OrgID, EnvID: t.Scope.EnvID, APIKey: t.APIKey, KeyIssued: t.KeyIssued,
	}, nil
}

// EnsureTenant is Notify.EnsureTenant on the running notify. It returns
// ErrNotRunning before Start and after Stop.
func (p *Program) EnsureTenant(ctx context.Context, spec TenantSpec) (Tenant, error) {
	n := p.notify()
	if n == nil {
		return Tenant{}, ErrNotRunning
	}
	return n.EnsureTenant(ctx, spec)
}
