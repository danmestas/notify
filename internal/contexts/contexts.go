// Package contexts is a thin service layer over store.ContextRepo.
package contexts

import (
	"context"
	"encoding/json"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// Service wraps the context repository for use by httpapi and console.
type Service struct {
	repo store.ContextRepo
}

// New creates a Service backed by the given store.
func New(st *store.Store) *Service {
	return &Service{repo: st.Contexts()}
}

func (svc *Service) Create(ctx context.Context, scope domain.Scope, ctype, externalID string, data json.RawMessage) (domain.Context, error) {
	return svc.repo.Create(ctx, scope, ctype, externalID, data)
}

func (svc *Service) Get(ctx context.Context, scope domain.Scope, ctype, externalID string) (domain.Context, error) {
	return svc.repo.Get(ctx, scope, ctype, externalID)
}

func (svc *Service) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Context, error) {
	return svc.repo.GetByID(ctx, scope, id)
}

func (svc *Service) List(ctx context.Context, scope domain.Scope, ctype, search, after, before string, limit int) ([]domain.Context, string, string, error) {
	return svc.repo.List(ctx, scope, ctype, search, after, before, limit)
}

func (svc *Service) Update(ctx context.Context, scope domain.Scope, ctype, externalID string, data json.RawMessage) (domain.Context, error) {
	return svc.repo.Update(ctx, scope, ctype, externalID, data)
}

func (svc *Service) Delete(ctx context.Context, scope domain.Scope, ctype, externalID string) error {
	return svc.repo.Delete(ctx, scope, ctype, externalID)
}
