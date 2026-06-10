// Package analytics owns notify's delivery-status analytics: a projector that
// drains every environment's delivery-status events off the event bus into the
// analytics store, plus a Scope-first query API and its HTTP route. The
// projector is best-effort — a malformed event or a transient store error is
// dropped, never panicked on, so the bus subscription keeps draining.
package analytics

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// Service is the analytics projector + query API. It reads delivery-status
// events from bus and persists/aggregates them through store.
type Service struct {
	repo store.AnalyticsRepo
	bus  *eventbus.Bus
}

// New builds an analytics Service bound to st and bus.
func New(s *store.Store, bus *eventbus.Bus) *Service {
	return &Service{repo: s.Analytics(), bus: bus}
}

// NewWithRepo builds a Service against an explicit AnalyticsRepo, letting the
// caller substitute the analytics backend (e.g. a DuckDB OLAP store) for the
// default SQLite one without changing the projector or the query API.
func NewWithRepo(repo store.AnalyticsRepo, bus *eventbus.Bus) *Service {
	return &Service{repo: repo, bus: bus}
}

// Start launches the projector: it subscribes to EVERY environment's
// delivery-status events and, for each, inserts one AnalyticsDelivery scoped to
// the event's OrgID/EnvID. The returned stop func unsubscribes.
func (svc *Service) Start(ctx context.Context) (stop func() error, err error) {
	unsub, err := svc.bus.SubscribeAllDelivery(func(data []byte) {
		svc.ingest(ctx, data)
	})
	if err != nil {
		return nil, err
	}
	return unsub, nil
}

// ingest unmarshals one delivery-status event (a domain.Message JSON) and
// appends an AnalyticsDelivery row when it carries a tenancy scope. Best-effort:
// errors are logged and swallowed so the subscription keeps draining.
func (svc *Service) ingest(ctx context.Context, data []byte) {
	var ev struct {
		OrgID         string
		EnvID         string
		Channel       string
		Status        string
		SubscriberID  string
		WorkflowID    string
		TransactionID string
		CreatedAt     string
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		log.Printf("analytics: drop event: unmarshal: %v", err)
		return
	}
	if ev.OrgID == "" || ev.EnvID == "" {
		return
	}
	scope := domain.Scope{OrgID: ev.OrgID, EnvID: ev.EnvID}
	if err := svc.repo.Insert(ctx, scope, domain.AnalyticsDelivery{
		Channel:       ev.Channel,
		Status:        ev.Status,
		SubscriberID:  ev.SubscriberID,
		WorkflowID:    ev.WorkflowID,
		TransactionID: ev.TransactionID,
		CreatedAt:     ev.CreatedAt,
	}); err != nil {
		log.Printf("analytics: insert: %v", err)
	}
}

// DeliveriesByChannel returns delivery counts grouped by channel then status
// for scope.
func (svc *Service) DeliveriesByChannel(ctx context.Context, scope domain.Scope) (map[string]map[string]int, error) {
	return svc.repo.CountByChannelStatus(ctx, scope)
}

// DeliveriesOverTime returns the per-day delivery time-series for scope.
func (svc *Service) DeliveriesOverTime(ctx context.Context, scope domain.Scope) ([]domain.DeliveryDayCount, error) {
	return svc.repo.DeliveriesByDay(ctx, scope)
}

// Routes mounts the analytics query API. Callers MUST mount it inside the
// identity.Middleware group so handlers resolve their Scope.
func (svc *Service) Routes(r chi.Router) {
	r.Get("/v1/analytics/deliveries", svc.getDeliveries)
	r.Get("/v1/analytics/timeseries", svc.getTimeseries)
}

// getDeliveries returns the channel/status delivery counts for the request
// Scope as JSON.
func (svc *Service) getDeliveries(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	m, err := svc.DeliveriesByChannel(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(m)
}

// getTimeseries returns the per-day delivery time-series for the request Scope
// as a JSON array.
func (svc *Service) getTimeseries(w http.ResponseWriter, r *http.Request) {
	scope := identity.ScopeFrom(r.Context())
	series, err := svc.DeliveriesOverTime(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(series)
}
