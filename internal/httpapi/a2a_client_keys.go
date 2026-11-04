package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/a2aauth"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// clientKeyDTO is the wire shape of a client key. It never carries the secret
// or its hash; the secret appears only in the mint response.
type clientKeyDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SubscriberID string `json:"subscriberId,omitempty"`
	CreatedAt    string `json:"createdAt"`
	LastUsedAt   string `json:"lastUsedAt,omitempty"`
	RevokedAt    string `json:"revokedAt,omitempty"`
}

func toClientKeyDTO(k domain.A2AClientKey) clientKeyDTO {
	return clientKeyDTO{
		ID: k.ID, Name: k.Name, SubscriberID: k.SubscriberID,
		CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt,
	}
}

// mintedClientKeyDTO is the POST response: the key plus its secret, shown once.
type mintedClientKeyDTO struct {
	clientKeyDTO
	Secret string `json:"secret"`
}

// postA2AClientKey handles POST /v1/a2a/client-keys.
func postA2AClientKey(svc *a2aauth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name         string `json:"name"`
			SubscriberID string `json:"subscriberId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		key, token, err := svc.Mint(r.Context(), identity.ScopeFrom(r.Context()), req.Name, req.SubscriberID)
		switch {
		case errors.Is(err, a2aauth.ErrInvalidName):
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
			return
		case errors.Is(err, a2aauth.ErrSubscriberNotFound):
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "subscriber not found")
			return
		case err != nil:
			writeInternalError(w, r, err)
			return
		}
		// A credential must never be cached by an intermediary.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, mintedClientKeyDTO{clientKeyDTO: toClientKeyDTO(key), Secret: token})
	}
}

// getA2AClientKeys handles GET /v1/a2a/client-keys.
func getA2AClientKeys(svc *a2aauth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := svc.List(r.Context(), identity.ScopeFrom(r.Context()))
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		out := make([]clientKeyDTO, 0, len(keys))
		for _, k := range keys {
			out = append(out, toClientKeyDTO(k))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// deleteA2AClientKey handles DELETE /v1/a2a/client-keys/{id} (revoke).
func deleteA2AClientKey(svc *a2aauth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := svc.Revoke(r.Context(), identity.ScopeFrom(r.Context()), chi.URLParam(r, "id"))
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "client key not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
