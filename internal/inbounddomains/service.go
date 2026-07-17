// Package inbounddomains provides the inbound email domain management service.
package inbounddomains

import (
	"context"
	"net"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

const (
	defaultMXHost     = "inbound.notify.example.com"
	defaultMXPriority = 10
)

// MXLookupFunc is the signature for an MX record lookup. Injectable for tests.
type MXLookupFunc func(host string) ([]*net.MX, error)

// Service manages inbound email domains.
type Service struct {
	st       *store.Store
	mxHost   string
	mxPrio   int
	lookupMX MXLookupFunc
}

// New creates a Service. The expected MX host is injected by the caller
// (cmd/notify reads NOTIFY_INBOUND_MX_HOST once); an empty mxHost falls back to
// defaultMXHost so callers without a configured host keep the prior behavior.
func New(st *store.Store, mxHost string) *Service {
	if mxHost == "" {
		mxHost = defaultMXHost
	}
	return &Service{
		st:       st,
		mxHost:   mxHost,
		mxPrio:   defaultMXPriority,
		lookupMX: net.LookupMX,
	}
}

// newWithLookup creates a Service with an injected MX lookup function (for tests).
func newWithLookup(st *store.Store, mxHost string, lookupMX MXLookupFunc) *Service {
	return &Service{
		st:       st,
		mxHost:   mxHost,
		mxPrio:   defaultMXPriority,
		lookupMX: lookupMX,
	}
}

// Create registers a new inbound domain for a scope (status=pending).
func (svc *Service) Create(ctx context.Context, scope domain.Scope, name string) (domain.InboundDomain, error) {
	return svc.st.InboundDomains().Create(ctx, scope, name, svc.mxHost, svc.mxPrio)
}

// Get returns a single domain by name within scope.
func (svc *Service) Get(ctx context.Context, scope domain.Scope, name string) (domain.InboundDomain, error) {
	return svc.st.InboundDomains().Get(ctx, scope, name)
}

// List returns domains in scope, keyset-paginated.
func (svc *Service) List(ctx context.Context, scope domain.Scope, search, after string, limit int) ([]domain.InboundDomain, string, int, error) {
	return svc.st.InboundDomains().List(ctx, scope, search, after, limit)
}

// Verify performs a live DNS MX lookup for the domain name and updates
// its status based on whether the expected MX host is present.
// Returns the updated domain.
func (svc *Service) Verify(ctx context.Context, scope domain.Scope, name string) (domain.InboundDomain, error) {
	d, err := svc.st.InboundDomains().Get(ctx, scope, name)
	if err != nil {
		return domain.InboundDomain{}, err
	}

	mxs, err := svc.lookupMX(name)
	verified := false
	if err == nil {
		expected := strings.ToLower(strings.TrimSuffix(d.ExpectedMXHost, "."))
		for _, mx := range mxs {
			host := strings.ToLower(strings.TrimSuffix(mx.Host, "."))
			if host == expected {
				verified = true
				break
			}
		}
	}

	status := "failed"
	if verified {
		status = "verified"
	}
	if err := svc.st.InboundDomains().SetStatus(ctx, scope, name, status, verified); err != nil {
		return domain.InboundDomain{}, err
	}
	d.Status = status
	d.MXConfigured = verified
	return d, nil
}

// Delete removes an inbound domain from scope.
func (svc *Service) Delete(ctx context.Context, scope domain.Scope, name string) error {
	return svc.st.InboundDomains().Delete(ctx, scope, name)
}

// MXHost returns the expected MX host for this installation.
func (svc *Service) MXHost() string { return svc.mxHost }

// MXPriority returns the expected MX priority.
func (svc *Service) MXPriority() int { return svc.mxPrio }
