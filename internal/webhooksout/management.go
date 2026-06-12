package webhooksout

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ManagementService provides CRUD operations over WebhookEndpoints, wrapping
// the store repo. It is a thin service layer whose main purpose is to give the
// HTTP handlers a stable seam and a home for any future business rules (e.g.
// URL validation, endpoint limits per scope).
type ManagementService struct {
	st *store.Store
}

// NewManagement builds a ManagementService over the given store.
func NewManagement(st *store.Store) *ManagementService {
	return &ManagementService{st: st}
}

// Create inserts a new webhook endpoint for the scope with the given URL.
// Newly created endpoints are inactive by default; the caller may pass
// active=true to enable them immediately.
func (svc *ManagementService) Create(ctx context.Context, scope domain.Scope, url string, active bool) (domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    url,
		Active: active,
	})
}

// List returns all endpoints (active and inactive) for the scope.
func (svc *ManagementService) List(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().List(ctx, scope)
}

// GetByID returns a single endpoint by its internal ULID id within scope.
func (svc *ManagementService) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().GetByID(ctx, scope, id)
}

// Update replaces the URL and optionally the active flag. The active pointer
// is nil-safe: when nil, the existing active state is preserved.
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

// Delete permanently removes an endpoint.
func (svc *ManagementService) Delete(ctx context.Context, scope domain.Scope, id string) error {
	return svc.st.WebhookEndpoints().Delete(ctx, scope, id)
}
