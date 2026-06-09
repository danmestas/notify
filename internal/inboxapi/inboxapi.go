// Package inboxapi serves the read-only in-app message feed. It mirrors the reference
// messages.controller GET ” endpoint: list a subscriber's messages filtered by
// channel, scoped to the tenant carried on the request context.
package inboxapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// Service exposes the in-app message feed over HTTP. Read-only: it never writes
// to the store.
type Service struct {
	store *store.Store
}

// New builds a Service backed by the given store.
func New(s *store.Store) *Service {
	return &Service{store: s}
}

// Routes registers the feed endpoints on r. It expects to run behind
// identity.Middleware so the request context carries a tenancy Scope.
func (svc *Service) Routes(r chi.Router) {
	r.Get("/v1/messages", svc.listMessages)
}

// listMessages handles GET /v1/messages?subscriberId=&channel=&transactionId=.
func (svc *Service) listMessages(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()
	subscriberID := q.Get("subscriberId")
	channel := q.Get("channel")
	transactionID := q.Get("transactionId")

	msgs, err := svc.store.Messages().List(r.Context(), scope, subscriberID, channel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := msgs
	if transactionID != "" {
		out = out[:0]
		for _, m := range msgs {
			if m.TransactionID == transactionID {
				out = append(out, m)
			}
		}
	}
	if out == nil {
		out = []domain.Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
