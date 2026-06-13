package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/store"
)

type inboundDomainDTO struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Status             string `json:"status"`
	MXConfigured       bool   `json:"mxConfigured"`
	ExpectedMXHost     string `json:"expectedMXHost"`
	ExpectedMXPriority int    `json:"expectedMXPriority"`
	CreatedAt          string `json:"createdAt"`
	UpdatedAt          string `json:"updatedAt"`
}

func toInboundDomainDTO(d domain.InboundDomain) inboundDomainDTO {
	return inboundDomainDTO{
		ID:                 d.ID,
		Name:               d.Name,
		Status:             d.Status,
		MXConfigured:       d.MXConfigured,
		ExpectedMXHost:     d.ExpectedMXHost,
		ExpectedMXPriority: d.ExpectedMXPriority,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}
}

type inboundDomainCreateRequest struct {
	Name string `json:"name"`
}

// getInboundDomains handles GET /v1/inbound/domains.
func getInboundDomains(svc *inbounddomains.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		q := r.URL.Query()
		search := q.Get("search")
		after := q.Get("after")
		limit := 20
		if l := q.Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		items, next, total, err := svc.List(r.Context(), scope, search, after, limit)
		if err != nil {
			http.Error(w, "failed to list inbound domains", http.StatusInternalServerError)
			return
		}
		dtos := make([]inboundDomainDTO, 0, len(items))
		for _, d := range items {
			dtos = append(dtos, toInboundDomainDTO(d))
		}
		type response struct {
			Data       []inboundDomainDTO `json:"data"`
			Next       string             `json:"next,omitempty"`
			TotalCount int                `json:"totalCount"`
		}
		writeJSON(w, http.StatusOK, response{Data: dtos, Next: next, TotalCount: total})
	}
}

// postInboundDomain handles POST /v1/inbound/domains.
func postInboundDomain(svc *inbounddomains.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req inboundDomainCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		d, err := svc.Create(r.Context(), scope, req.Name)
		if err != nil {
			if errors.Is(err, store.ErrInboundDomainConflict) {
				http.Error(w, "inbound domain already exists", http.StatusConflict)
				return
			}
			http.Error(w, "failed to create inbound domain", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, toInboundDomainDTO(d))
	}
}

// getInboundDomain handles GET /v1/inbound/domains/{name}.
func getInboundDomain(svc *inbounddomains.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		scope := identity.ScopeFrom(r.Context())
		d, err := svc.Get(r.Context(), scope, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "inbound domain not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to get inbound domain", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toInboundDomainDTO(d))
	}
}

// postInboundDomainVerify handles POST /v1/inbound/domains/{name}/verify.
func postInboundDomainVerify(svc *inbounddomains.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		scope := identity.ScopeFrom(r.Context())
		d, err := svc.Verify(r.Context(), scope, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "inbound domain not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to verify inbound domain", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toInboundDomainDTO(d))
	}
}

// deleteInboundDomain handles DELETE /v1/inbound/domains/{name}.
func deleteInboundDomain(svc *inbounddomains.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		scope := identity.ScopeFrom(r.Context())
		if err := svc.Delete(r.Context(), scope, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "inbound domain not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete inbound domain", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
