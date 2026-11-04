package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/a2areceiver"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// receiverKeyDTO is the wire shape of a receiver key. It never carries the
// secret or its hash; the secret appears only in the mint response.
type receiverKeyDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
	RevokedAt  string `json:"revokedAt,omitempty"`
}

func toReceiverKeyDTO(k domain.A2AReceiverKey) receiverKeyDTO {
	return receiverKeyDTO{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

// postReceiverKey handles POST /v1/agents/{identifier}/receiver-keys.
func postReceiverKey(svc *a2areceiver.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		key, token, err := svc.Mint(r.Context(), identity.ScopeFrom(r.Context()), chi.URLParam(r, "identifier"), req.Name)
		switch {
		case errors.Is(err, a2areceiver.ErrInvalidName):
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
			return
		case errors.Is(err, store.ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			return
		case err != nil:
			writeInternalError(w, r, err)
			return
		}
		// A credential must never be cached by an intermediary.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, struct {
			receiverKeyDTO
			Secret string `json:"secret"`
		}{toReceiverKeyDTO(key), token})
	}
}

// getReceiverKeys handles GET /v1/agents/{identifier}/receiver-keys.
func getReceiverKeys(svc *a2areceiver.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := svc.List(r.Context(), identity.ScopeFrom(r.Context()), chi.URLParam(r, "identifier"))
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		out := make([]receiverKeyDTO, 0, len(keys))
		for _, k := range keys {
			out = append(out, toReceiverKeyDTO(k))
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// deleteReceiverKey handles DELETE /v1/agents/{identifier}/receiver-keys/{id}
// (revoke). A receiver connected with the key is disconnected.
func deleteReceiverKey(svc *a2areceiver.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := svc.Revoke(r.Context(), identity.ScopeFrom(r.Context()), chi.URLParam(r, "identifier"), chi.URLParam(r, "id"))
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "receiver key not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
