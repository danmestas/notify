package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

var (
	// ErrTenantSpec marks a tenant spec whose organization or environment is
	// empty once normalised.
	ErrTenantSpec = errors.New("identity: organization and environment are required")
	// ErrTenantConflict marks a spec that names more than one existing
	// organization, so no single tenant can be chosen for it.
	ErrTenantConflict = errors.New("identity: tenant spec is ambiguous")
)

// Tenant is the outcome of EnsureTenant.
type Tenant struct {
	Scope domain.Scope
	// APIKey is the plaintext key, set only when KeyIssued. It is never
	// recoverable afterwards except via rotation.
	APIKey string
	// KeyIssued reports that this call minted APIKey: the environment was new,
	// or it existed without any key (left by an interrupted older bootstrap).
	KeyIssued bool
}

// envIdentifier is the slug an environment's display name maps to. It is the
// one rule shared by CreateEnvironment and EnsureTenant.
func envIdentifier(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
}

// EnsureTenant is the idempotent form of Bootstrap: it finds the environment
// named envName under the organization named orgName, creating the
// organization and/or environment when absent, and mints an API key whenever
// the environment has none. Everything runs in one BEGIN IMMEDIATE transaction,
// so a crash leaves nothing half-made and concurrent callers, in this process
// or another sharing the database file, agree on one org, env and key.
// Two organizations sharing orgName yield ErrTenantConflict.
func (svc *Service) EnsureTenant(ctx context.Context, orgName, envName string) (Tenant, error) {
	orgName, envName = strings.TrimSpace(orgName), strings.TrimSpace(envName)
	identifier := envIdentifier(envName)
	if orgName == "" || identifier == "" {
		return Tenant{}, ErrTenantSpec
	}
	var out Tenant
	err := svc.store.WithImmediateTx(ctx, func(tx *store.Tx) error {
		var err error
		out, err = svc.ensureInTx(ctx, tx, orgName, envName, identifier)
		return err
	})
	if err != nil {
		return Tenant{}, err
	}
	return out, nil
}

func (svc *Service) ensureInTx(ctx context.Context, tx *store.Tx, orgName, envName, identifier string) (Tenant, error) {
	orgID, err := findOrCreateOrg(ctx, tx, orgName)
	if err != nil {
		return Tenant{}, err
	}
	envs, err := tx.Environments().List(ctx, orgID)
	if err != nil {
		return Tenant{}, fmt.Errorf("identity: list envs: %w", err)
	}
	var scope domain.Scope
	found := false
	for _, env := range envs {
		if env.Identifier == identifier {
			scope, found = domain.Scope{OrgID: env.OrgID, EnvID: env.ID}, true
			break
		}
	}
	if !found {
		_, scope, err = tx.Environments().Create(ctx, orgID, domain.Environment{
			Name: envName, Identifier: identifier,
		})
		if err != nil {
			return Tenant{}, fmt.Errorf("identity: create env: %w", err)
		}
	} else {
		keys, err := tx.ApiKeys().List(ctx, scope)
		if err != nil {
			return Tenant{}, fmt.Errorf("identity: list api keys: %w", err)
		}
		if len(keys) > 0 {
			return Tenant{Scope: scope}, nil
		}
	}
	raw, err := svc.createKey(ctx, tx, scope)
	if err != nil {
		return Tenant{}, err
	}
	return Tenant{Scope: scope, APIKey: raw, KeyIssued: true}, nil
}

func findOrCreateOrg(ctx context.Context, tx *store.Tx, name string) (string, error) {
	orgs, err := tx.Organizations().FindByName(ctx, name)
	if err != nil {
		return "", fmt.Errorf("identity: find org: %w", err)
	}
	switch len(orgs) {
	case 0:
		org, err := tx.Organizations().Create(ctx, name)
		if err != nil {
			return "", fmt.Errorf("identity: create org: %w", err)
		}
		return org.ID, nil
	case 1:
		return orgs[0].ID, nil
	default:
		return "", fmt.Errorf("%w: %d organizations are named %q", ErrTenantConflict, len(orgs), name)
	}
}

// createKey mints a key for scope on tx and returns its plaintext hex.
func (svc *Service) createKey(ctx context.Context, tx *store.Tx, scope domain.Scope) (string, error) {
	raw := make([]byte, keyLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: mint key: %w", err)
	}
	enc, err := svc.encrypt(raw)
	if err != nil {
		return "", fmt.Errorf("identity: encrypt key: %w", err)
	}
	if _, err := tx.ApiKeys().Create(ctx, scope, domain.APIKey{Hash: hashKey(raw), EncKey: enc}); err != nil {
		return "", fmt.Errorf("identity: create api key: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
