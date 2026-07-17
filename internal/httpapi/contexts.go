package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/contexts"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// contextDTO is the JSON shape for context responses.
type contextDTO struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	ExternalID string          `json:"externalId"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  string          `json:"createdAt"`
	UpdatedAt  string          `json:"updatedAt"`
}

func toContextDTO(c domain.Context) contextDTO {
	data := c.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return contextDTO{
		ID: c.ID, Type: c.Type, ExternalID: c.ExternalID,
		Data: data, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// getContexts handles GET /v1/contexts.
func getContexts(svc *contexts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		items, next, prev, err := svc.List(r.Context(), scope,
			q.Get("type"), q.Get("search"), q.Get("after"), q.Get("before"), limit)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		dtos := make([]contextDTO, 0, len(items))
		for _, c := range items {
			dtos = append(dtos, toContextDTO(c))
		}
		type response struct {
			Data       []contextDTO `json:"data"`
			NextCursor string       `json:"nextCursor,omitempty"`
			PrevCursor string       `json:"prevCursor,omitempty"`
		}
		writeJSON(w, http.StatusOK, response{Data: dtos, NextCursor: next, PrevCursor: prev})
	}
}

// postContextRequest is the body for POST /v1/contexts.
type postContextRequest struct {
	Type       string          `json:"type"`
	ExternalID string          `json:"id"`
	Data       json.RawMessage `json:"data"`
}

// postContext handles POST /v1/contexts.
func postContext(svc *contexts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req postContextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.Type == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "type is required")
			return
		}
		if req.ExternalID == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "id is required")
			return
		}
		if len(req.Data) > 0 && !json.Valid(req.Data) {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "data must be valid JSON")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		c, err := svc.Create(r.Context(), scope, req.Type, req.ExternalID, req.Data)
		if err != nil {
			if errors.Is(err, store.ErrContextConflict) {
				writeJSONError(w, http.StatusConflict, "conflict", "context with this type and id already exists")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, toContextDTO(c))
	}
}

// getContext handles GET /v1/contexts/{type}/{id}.
func getContext(svc *contexts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctype := chi.URLParam(r, "type")
		externalID := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		c, err := svc.Get(r.Context(), scope, ctype, externalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "context not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toContextDTO(c))
	}
}

// putContextRequest is the body for PUT /v1/contexts/{type}/{id}.
type putContextRequest struct {
	Data json.RawMessage `json:"data"`
}

// putContext handles PUT /v1/contexts/{type}/{id}.
func putContext(svc *contexts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctype := chi.URLParam(r, "type")
		externalID := chi.URLParam(r, "id")
		var req putContextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if len(req.Data) > 0 && !json.Valid(req.Data) {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "data must be valid JSON")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		c, err := svc.Update(r.Context(), scope, ctype, externalID, req.Data)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "context not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toContextDTO(c))
	}
}

// deleteContext handles DELETE /v1/contexts/{type}/{id}.
func deleteContext(svc *contexts.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctype := chi.URLParam(r, "type")
		externalID := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		if err := svc.Delete(r.Context(), scope, ctype, externalID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "context not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
