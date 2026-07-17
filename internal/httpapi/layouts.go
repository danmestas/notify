package httpapi

// layouts.go implements the /v1/layouts management surface: list, create, get,
// update (upsert), delete. Every handler resolves its tenancy Scope via
// identity.ScopeFrom and accesses the store directly through LayoutRepo. The
// routes are mounted only when a store is present (see New).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// layoutDTO is the wire shape for a single layout.
type layoutDTO struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Name    string `json:"name"`
	Content string `json:"content"`
	OrgID   string `json:"orgId"`
	EnvID   string `json:"envId"`
}

func toLayoutDTO(l domain.Layout) layoutDTO {
	return layoutDTO{
		ID:      l.ID,
		Key:     l.Key,
		Name:    l.Name,
		Content: l.Content,
		OrgID:   l.OrgID,
		EnvID:   l.EnvID,
	}
}

// defaultLayoutPageLimit caps an unbounded list request.
const defaultLayoutPageLimit = 50

// getLayouts handles GET /v1/layouts?after=key&limit=N.
func getLayouts(repo store.LayoutRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		after := r.URL.Query().Get("after")
		limit := defaultLayoutPageLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				limit = n
			}
		}
		layouts, nextKey, err := repo.List(r.Context(), scope, after, limit)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		dtos := make([]layoutDTO, 0, len(layouts))
		for _, l := range layouts {
			dtos = append(dtos, toLayoutDTO(l))
		}
		type response struct {
			Layouts []layoutDTO `json:"layouts"`
			NextKey string      `json:"nextKey,omitempty"`
		}
		writeJSON(w, http.StatusOK, response{Layouts: dtos, NextKey: nextKey})
	}
}

// layoutCreateRequest is the body for POST /v1/layouts.
type layoutCreateRequest struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// postLayout handles POST /v1/layouts.
func postLayout(repo store.LayoutRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req layoutCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.Key == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "key is required")
			return
		}
		if req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "name is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		l, err := repo.Create(r.Context(), scope, req.Key, req.Name, req.Content)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeJSONError(w, http.StatusConflict, "conflict", "layout key already exists")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, toLayoutDTO(l))
	}
}

// getLayout handles GET /v1/layouts/{key}.
func getLayout(repo store.LayoutRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		l, err := repo.Get(r.Context(), scope, key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "layout not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toLayoutDTO(l))
	}
}

// layoutUpdateRequest is the body for PUT /v1/layouts/{key}.
type layoutUpdateRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// putLayout handles PUT /v1/layouts/{key}. The update path is an upsert: it
// writes name+content for the key, creating the row if absent.
func putLayout(repo store.LayoutRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		var req layoutUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := repo.Upsert(r.Context(), scope, key, req.Name, req.Content); err != nil {
			writeInternalError(w, r, err)
			return
		}
		l, err := repo.Get(r.Context(), scope, key)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toLayoutDTO(l))
	}
}

// deleteLayout handles DELETE /v1/layouts/{key}.
func deleteLayout(repo store.LayoutRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if err := repo.Delete(r.Context(), scope, key); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "layout not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
