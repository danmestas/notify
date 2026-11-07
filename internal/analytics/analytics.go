// Package analytics owns notify's delivery-status analytics: a projector that
// drains every environment's delivery-status events off the event bus into the
// analytics store, plus a Scope-first query API and its HTTP route. The
// projector is a durable JetStream consumer, so it resumes after downtime; a
// malformed event is dropped and a transient store error is retried, never
// panicked on, so the consumer keeps draining.
package analytics

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

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

// durableName is the JetStream durable consumer the projector resumes across
// restarts. Renaming it starts a fresh consumer with no memory of what was
// already projected, so do not change it without a migration plan.
const durableName = "notify-analytics"

// Start launches the projector: a durable JetStream consumer over EVERY
// environment's delivery-status events that inserts one AnalyticsDelivery
// scoped to the event's OrgID/EnvID. Events published while notify is down are
// delivered on the next Start. The returned stop func drains the consumer.
func (svc *Service) Start(ctx context.Context) (stop func() error, err error) {
	return svc.bus.ConsumeAllDeliveryDurable(ctx, durableName, func(data []byte) error {
		return svc.ingest(ctx, data)
	})
}

// ingest unmarshals one delivery-status event (a domain.Message JSON) and
// appends an AnalyticsDelivery row when it carries a tenancy scope. A malformed
// or unscoped event is logged and dropped (nil: acked, never redelivered). A
// store error is returned so the durable consumer Naks and redelivers; the
// event key makes that redelivery idempotent.
func (svc *Service) ingest(ctx context.Context, data []byte) error {
	var ev struct {
		OrgID         string
		EnvID         string
		Channel       string
		Status        string
		SubscriberID  string
		WorkflowID    string
		TransactionID string
		RunID         string
		CreatedAt     string
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		log.Printf("analytics: drop event: unmarshal: %v", err)
		return nil
	}
	if ev.OrgID == "" || ev.EnvID == "" {
		log.Printf("analytics: drop event: no tenancy scope")
		return nil
	}
	scope := domain.Scope{OrgID: ev.OrgID, EnvID: ev.EnvID}
	if err := svc.repo.Insert(ctx, scope, domain.AnalyticsDelivery{
		Channel:       ev.Channel,
		Status:        ev.Status,
		SubscriberID:  ev.SubscriberID,
		WorkflowID:    ev.WorkflowID,
		TransactionID: ev.TransactionID,
		RunID:         ev.RunID,
		CreatedAt:     ev.CreatedAt,
		EventKey:      eventbus.DeliveryEventKey(data),
	}); err != nil {
		log.Printf("analytics: insert: %v", err)
		return err
	}
	return nil
}

// DeliveriesByChannel returns delivery counts grouped by channel then status
// for scope, optionally filtered by workflow_id (GAP-AF-06).
func (svc *Service) DeliveriesByChannel(ctx context.Context, scope domain.Scope, f store.AnalyticsFilter) (map[string]map[string]int, error) {
	return svc.repo.CountByChannelStatus(ctx, scope, f)
}

// DeliveriesOverTime returns the per-day delivery time-series for scope,
// optionally filtered by workflow_id (GAP-AF-06).
func (svc *Service) DeliveriesOverTime(ctx context.Context, scope domain.Scope, f store.AnalyticsFilter) ([]domain.DeliveryDayCount, error) {
	return svc.repo.DeliveriesByDay(ctx, scope, f)
}

// CountActiveSubscribers returns the number of distinct subscribers with at
// least one delivery row at or after since, optionally scoped to a single
// workflow via f.WorkflowID (GAP-AF-05, M1).
func (svc *Service) CountActiveSubscribers(ctx context.Context, scope domain.Scope, since time.Time, f store.AnalyticsFilter) (int64, error) {
	return svc.repo.CountActiveSubscribers(ctx, scope, since, f)
}

// AverageMessagesPerSubscriber returns the average delivery count per distinct
// subscriber at or after since, optionally scoped to a single workflow via
// f.WorkflowID (GAP-AF-05, M1). Returns 0 when no data.
func (svc *Service) AverageMessagesPerSubscriber(ctx context.Context, scope domain.Scope, since time.Time, f store.AnalyticsFilter) (float64, error) {
	return svc.repo.AverageMessagesPerSubscriber(ctx, scope, since, f)
}

// RunsOverTime returns the per-day time-series of DISTINCT workflow runs for
// scope, optionally filtered by workflow_id. Backs the Workflow Runs Trend
// chart — a run fanned out to N channels counts once, not N times (M2).
func (svc *Service) RunsOverTime(ctx context.Context, scope domain.Scope, f store.AnalyticsFilter) ([]domain.RunDayCount, error) {
	return svc.repo.RunsByDay(ctx, scope, f)
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
	m, err := svc.DeliveriesByChannel(r.Context(), scope, store.AnalyticsFilter{})
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
	series, err := svc.DeliveriesOverTime(r.Context(), scope, store.AnalyticsFilter{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(series)
}
