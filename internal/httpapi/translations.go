package httpapi

// translations.go implements the /v1/translations management surface: list
// locales with key counts, read a locale's full key map, upsert a single key,
// bulk-replace a locale's map, and delete a key. Every handler resolves its
// tenancy Scope via identity.ScopeFrom and accesses the store directly through
// TranslationRepo. The routes are mounted only when a store is present.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// localeStatDTO is one element of the GET /v1/translations response.
type localeStatDTO struct {
	Locale   string `json:"locale"`
	KeyCount int    `json:"keyCount"`
}

// getTranslations handles GET /v1/translations: list locales with key counts.
func getTranslations(repo store.TranslationRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		stats, err := repo.LocaleStats(r.Context(), scope)
		if err != nil {
			http.Error(w, "failed to list translations", http.StatusInternalServerError)
			return
		}
		dtos := make([]localeStatDTO, 0, len(stats))
		for _, s := range stats {
			dtos = append(dtos, localeStatDTO{Locale: s.Locale, KeyCount: s.KeyCount})
		}
		type response struct {
			Locales []localeStatDTO `json:"locales"`
		}
		writeJSON(w, http.StatusOK, response{Locales: dtos})
	}
}

// getTranslation handles GET /v1/translations/{locale}: the locale's full
// key map as a JSON object. Returns 404 when the locale has no keys.
func getTranslation(repo store.TranslationRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locale := chi.URLParam(r, "locale")
		scope := identity.ScopeFrom(r.Context())
		m, err := repo.Map(r.Context(), scope, locale)
		if err != nil {
			http.Error(w, "failed to get translations", http.StatusInternalServerError)
			return
		}
		if len(m) == 0 {
			http.Error(w, "locale not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

// translationCreateRequest is the body for POST /v1/translations.
type translationCreateRequest struct {
	Locale string `json:"locale"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

// postTranslation handles POST /v1/translations: upsert a single key.
func postTranslation(repo store.TranslationRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req translationCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Locale == "" {
			http.Error(w, "locale is required", http.StatusBadRequest)
			return
		}
		if req.Key == "" {
			http.Error(w, "key is required", http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := repo.Upsert(r.Context(), scope, req.Locale, req.Key, req.Value); err != nil {
			http.Error(w, "failed to create translation", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"locale": req.Locale,
			"key":    req.Key,
		})
	}
}

// translationKeyUpdateRequest is the body for PUT /v1/translations/{locale}/{key}.
type translationKeyUpdateRequest struct {
	Value string `json:"value"`
}

// putTranslationKey handles PUT /v1/translations/{locale}/{key}: upsert one key.
func putTranslationKey(repo store.TranslationRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locale := chi.URLParam(r, "locale")
		key := chi.URLParam(r, "key")
		var req translationKeyUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := repo.Upsert(r.Context(), scope, locale, key, req.Value); err != nil {
			http.Error(w, "failed to update translation", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"locale": locale,
			"key":    key,
			"value":  req.Value,
		})
	}
}

// putTranslationLocale handles PUT /v1/translations/{locale}: bulk-replace the
// locale's entire key map. The body is a raw JSON object {key:value,...}.
func putTranslationLocale(repo store.TranslationRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locale := chi.URLParam(r, "locale")
		var pairs map[string]string
		if err := json.NewDecoder(r.Body).Decode(&pairs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := identity.ScopeFrom(r.Context())
		if err := repo.BulkReplace(r.Context(), scope, locale, pairs); err != nil {
			http.Error(w, "failed to replace translations", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"locale":   locale,
			"keyCount": len(pairs),
		})
	}
}

// deleteTranslationKey handles DELETE /v1/translations/{locale}/{key}.
func deleteTranslationKey(repo store.TranslationRepo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locale := chi.URLParam(r, "locale")
		key := chi.URLParam(r, "key")
		scope := identity.ScopeFrom(r.Context())
		if err := repo.Delete(r.Context(), scope, locale, key); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "translation not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete translation", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
