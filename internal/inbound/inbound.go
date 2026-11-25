// Package inbound exposes a webhook-style ingestion path where external
// sources POST to a per-workflow URL (POST /v1/inbound/{trigger}). The path
// param selects the workflow trigger identifier; the body carries the
// recipient and payload. It runs behind identity.Middleware, so every request
// resolves an authenticated tenancy Scope.
package inbound

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/trigger"
)

// Service fires workflow triggers from inbound webhook requests.
type Service struct {
	trg *trigger.Service
}

// New builds an inbound Service over the trigger Service.
func New(trg *trigger.Service) *Service {
	return &Service{trg: trg}
}

// Routes mounts POST /v1/inbound/{trigger} on r. The route is expected to run
// behind identity.Middleware.
func (s *Service) Routes(r chi.Router) {
	r.Post("/v1/inbound/{trigger}", s.postInbound)
}

// postInbound decodes the body as a TriggerRequest, overrides its Name with the
// {trigger} path param (the URL selects the workflow), ingests it within the
// request Scope, and returns the TriggerResponse as JSON with 201.
func (s *Service) postInbound(w http.ResponseWriter, r *http.Request) {
	var req trigger.TriggerRequest
	body := http.MaxBytesReader(w, r.Body, trigger.MaxRequestBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body is larger than 15 MB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = chi.URLParam(r, "trigger")
	scope := identity.ScopeFrom(r.Context())
	resp, err := s.trg.Ingest(r.Context(), scope, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusCreated
	if resp.AttachmentsRefused {
		// The other refusals answer 201 on this route (read acknowledged), but
		// a refused file is the sender's to fix.
		status = http.StatusUnprocessableEntity
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
