// Package httpapi assembles notify's public HTTP surface: subscriber upsert,
// event trigger, and the in-app message feed, all mounted behind
// identity.Middleware so every handler runs with an authenticated tenancy
// Scope on its request context.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/version"
)

// New builds the notify HTTP handler. All routes are grouped behind
// id.Middleware, so each handler resolves its Scope via identity.ScopeFrom.
func New(id *identity.Service, aud *audience.Service, trg *trigger.Service, inbox *inboxapi.Service, rt *realtime.Gateway, inb *inbound.Service, an *analytics.Service, ag *agentsapi.Service, ing *ingress.Service) http.Handler {
	r := chi.NewRouter()
	// Liveness probe, mounted outside the auth group so container/orchestrator
	// health checks need no API key.
	r.Get("/healthz", healthz)
	r.Group(func(pr chi.Router) {
		pr.Use(id.Middleware)
		pr.Post("/v1/subscribers", postSubscriber(aud))
		pr.Post("/v1/events/trigger", postTrigger(trg))
		pr.Post("/v1/topics", postTopic(aud))
		pr.Post("/v1/topics/{key}/subscribers", postTopicSubscriber(aud))
		inbox.Routes(pr)
		if inb != nil {
			inb.Routes(pr)
		}
		if rt != nil {
			rt.Routes(pr)
		}
		if an != nil {
			an.Routes(pr)
		}
		if ag != nil {
			ag.Routes(pr)
		}
		if ing != nil {
			ing.Routes(pr)
		}
	})
	return r
}

// healthz is the unauthenticated liveness endpoint for container and
// orchestrator health checks: it reports 200 with the build version as JSON.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version.Version,
	})
}

// subscriberRequest is the POST /v1/subscribers body.
type subscriberRequest struct {
	SubscriberID string `json:"subscriberId"`
	Email        string `json:"email"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Phone        string `json:"phone"`
	Locale       string `json:"locale"`
}

// postSubscriber upserts a subscriber within the request Scope and returns the
// persisted row as JSON with 201.
func postSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req subscriberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		sub, err := aud.Upsert(r.Context(), scope, audience.SubscriberInput{
			SubscriberID: req.SubscriberID,
			Email:        req.Email,
			FirstName:    req.FirstName,
			LastName:     req.LastName,
			Phone:        req.Phone,
			Locale:       req.Locale,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, sub)
	}
}

// postTrigger ingests a trigger within the request Scope and returns the
// TriggerResponse as JSON with 201.
func postTrigger(trg *trigger.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req trigger.TriggerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		resp, err := trg.Ingest(r.Context(), scope, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// topicRequest is the POST /v1/topics body.
type topicRequest struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// postTopic creates (or upserts) a topic within the request Scope and returns
// the persisted row as JSON with 201.
func postTopic(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req topicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		topic, err := aud.CreateTopic(r.Context(), scope, req.Key, req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, topic)
	}
}

// topicSubscriberRequest is the POST /v1/topics/{key}/subscribers body.
type topicSubscriberRequest struct {
	SubscriberID string `json:"subscriberId"`
}

// postTopicSubscriber adds a subscriber to the named topic within the request
// Scope. An unknown topic key surfaces store.ErrNotFound as 404.
func postTopicSubscriber(aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req topicSubscriberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if err := aud.AddTopicSubscriber(r.Context(), scope, key, req.SubscriberID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
