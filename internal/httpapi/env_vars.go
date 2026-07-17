package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// envVarDTO is the JSON shape returned by env-var list/get endpoints.
// Value is always masked unless reveal=1 was requested.
type envVarDTO struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	IsSecret  bool   `json:"isSecret"`
	Value     string `json:"value"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// getEnvVars handles GET /v1/env-vars.
// Query params: ?search= ?after= ?before= ?limit= ?reveal=1
// Values are masked by default (ValueMaskSentinel). ?reveal=1 returns plaintext.
// Fail-closed: if reveal fails for any item, returns 500.
func getEnvVars(ev *envvars.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		reveal := q.Get("reveal") == "1"

		items, next, prev, err := ev.List(r.Context(), scope,
			q.Get("search"), q.Get("after"), q.Get("before"), limit)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}

		dtos := make([]envVarDTO, 0, len(items))
		for _, item := range items {
			value := envvars.ValueMaskSentinel
			if reveal {
				plain, err := ev.GetValue(r.Context(), scope, item.Key)
				if err != nil {
					// Fail-closed: any decrypt failure → 500, never leak.
					writeInternalError(w, r, err)
					return
				}
				value = plain
			}
			dtos = append(dtos, envVarDTO{
				ID:        item.ID,
				Key:       item.Key,
				IsSecret:  item.IsSecret,
				Value:     value,
				CreatedAt: item.CreatedAt,
				UpdatedAt: item.UpdatedAt,
			})
		}

		type response struct {
			Items      []envVarDTO `json:"items"`
			NextCursor string      `json:"nextCursor,omitempty"`
			PrevCursor string      `json:"prevCursor,omitempty"`
		}
		writeJSON(w, http.StatusOK, response{Items: dtos, NextCursor: next, PrevCursor: prev})
	}
}

// postEnvVarRequest is the body for POST /v1/env-vars.
type postEnvVarRequest struct {
	Key      string `json:"key"`
	IsSecret bool   `json:"isSecret"`
	Value    string `json:"value"`
}

// postEnvVar handles POST /v1/env-vars. Creates or upserts an env var.
// Returns 201 with masked DTO.
func postEnvVar(ev *envvars.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req postEnvVarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.Key == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "key is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		item, err := ev.Set(r.Context(), scope, req.Key, req.IsSecret, req.Value)
		if err != nil {
			if errors.Is(err, envvars.ErrInvalidKey) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, envVarDTO{
			ID:        item.ID,
			Key:       item.Key,
			IsSecret:  item.IsSecret,
			Value:     envvars.ValueMaskSentinel,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
		})
	}
}

// putEnvVarRequest is the body for PUT /v1/env-vars/{key}.
type putEnvVarRequest struct {
	IsSecret *bool  `json:"isSecret"`
	Value    string `json:"value"`
}

// putEnvVar handles PUT /v1/env-vars/{key}. Updates an env var.
// If value is empty or the sentinel, keeps the existing encrypted value.
// Returns 200 with masked DTO.
func putEnvVar(ev *envvars.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())

		var req putEnvVarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		// Resolve isSecret: use existing if not provided.
		existing, err := ev.Get(r.Context(), scope, key)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "env var not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}

		isSecret := existing.IsSecret
		if req.IsSecret != nil {
			isSecret = *req.IsSecret
		}

		// If value is empty or sentinel, re-encrypt the existing value to preserve it.
		value := req.Value
		if value == "" || value == envvars.ValueMaskSentinel {
			plain, err := ev.GetValue(r.Context(), scope, key)
			if err != nil {
				writeInternalError(w, r, err)
				return
			}
			value = plain
		}

		item, err := ev.Set(r.Context(), scope, key, isSecret, value)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, envVarDTO{
			ID:        item.ID,
			Key:       item.Key,
			IsSecret:  item.IsSecret,
			Value:     envvars.ValueMaskSentinel,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
		})
	}
}

// deleteEnvVar handles DELETE /v1/env-vars/{key}. Returns 204.
func deleteEnvVar(ev *envvars.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if err := ev.Delete(r.Context(), scope, key); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "env var not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
