package webhooksout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/store"
)

// ManagementService is the CRUD surface for an environment's webhook
// endpoints. It also owns the signing secret's lifecycle: minted on create,
// sealed at rest, shown once, rotatable.
type ManagementService struct {
	st  *store.Store
	box *secretbox.Box
}

// ErrSigningUnavailable reports a secret operation on a ManagementService built
// without a secretbox -- a wiring fault, not a tenant condition.
var ErrSigningUnavailable = errors.New("webhooksout: signing secrets are not configured")

// NewManagement builds the service. box seals signing secrets; nil disables
// them, which only tests should want -- an endpoint created without a box
// receives unsigned events.
func NewManagement(st *store.Store, box *secretbox.Box) *ManagementService {
	return &ManagementService{st: st, box: box}
}

// Created is the result of Create: the endpoint plus its signing secret in
// the clear. The secret is not stored in the clear and is not shown again;
// a caller that loses it rotates.
type Created struct {
	Endpoint domain.WebhookEndpoint
	Secret   string
}

// Create registers an endpoint and mints its signing secret. The secret is
// 32 random bytes as hex, sealed under the master key before it touches the
// store, and returned once.
func (svc *ManagementService) Create(ctx context.Context, scope domain.Scope, url string, active bool) (Created, error) {
	plain, sealed, err := svc.mint()
	if err != nil {
		return Created{}, err
	}
	ep, err := svc.st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    url,
		Active: active,
		Secret: sealed,
	})
	if err != nil {
		return Created{}, err
	}
	return Created{Endpoint: ep, Secret: plain}, nil
}

// RotateSecret mints a fresh signing secret for an existing endpoint and
// returns it once. Events sent after this call are signed with the new
// secret; there is no overlap window, so a consumer rotates by updating its
// verifier first and then calling this.
func (svc *ManagementService) RotateSecret(ctx context.Context, scope domain.Scope, id string) (Created, error) {
	if svc.box == nil {
		return Created{}, ErrSigningUnavailable
	}
	plain, sealed, err := svc.mint()
	if err != nil {
		return Created{}, err
	}
	ep, err := svc.st.WebhookEndpoints().SetSecret(ctx, scope, id, sealed)
	if err != nil {
		return Created{}, err
	}
	return Created{Endpoint: ep, Secret: plain}, nil
}

// mint returns a new secret in the clear and sealed. With no box the sealed
// form is empty, which the consumer reads as "unsigned endpoint".
func (svc *ManagementService) mint() (plain, sealed string, err error) {
	if svc.box == nil {
		return "", "", nil
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("webhooksout: mint secret: %w", err)
	}
	plain = hex.EncodeToString(raw[:])
	sealed, err = svc.box.Seal(plain)
	if err != nil {
		return "", "", fmt.Errorf("webhooksout: seal secret: %w", err)
	}
	return plain, sealed, nil
}

func (svc *ManagementService) List(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().List(ctx, scope)
}

func (svc *ManagementService) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().GetByID(ctx, scope, id)
}

func (svc *ManagementService) Update(ctx context.Context, scope domain.Scope, id, url string, active *bool) (domain.WebhookEndpoint, error) {
	ep, err := svc.st.WebhookEndpoints().UpdateURL(ctx, scope, id, url)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if active != nil {
		ep, err = svc.st.WebhookEndpoints().SetActive(ctx, scope, id, *active)
		if err != nil {
			return domain.WebhookEndpoint{}, err
		}
	}
	return ep, nil
}

func (svc *ManagementService) Delete(ctx context.Context, scope domain.Scope, id string) error {
	return svc.st.WebhookEndpoints().Delete(ctx, scope, id)
}
