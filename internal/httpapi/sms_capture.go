package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/smsloop"
	"github.com/danmestas/notify/internal/store"
)

const (
	capturedDefaultLimit = 50
	capturedMaxLimit     = 200
)

// getCapturedSMS handles GET /v1/integrations/sms/{integrationId}/captured:
// the texts a capture integration accepted, newest first.
func getCapturedSMS(conn *connectivity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := identity.ScopeFrom(r.Context())
		in, err := conn.Store().Integrations().GetByID(r.Context(), scope, chi.URLParam(r, "integrationId"))
		if errors.Is(err, store.ErrNotFound) || (err == nil && (in.Channel != "sms" || in.ProviderID != "capture")) {
			writeJSONError(w, http.StatusNotFound, "not_found", "capture integration not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		limit := capturedDefaultLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeJSONError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
				return
			}
			limit = min(n, capturedMaxLimit)
		}
		rows, err := conn.Store().SMSCaptured().List(r.Context(), scope, in.ID, limit)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		data := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			data = append(data, map[string]any{
				"id": c.ID, "providerMessageId": c.ProviderMessageID,
				"to": c.To, "from": c.From, "body": c.Body, "sentAt": c.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	}
}

type simulateInboundRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
	Body string `json:"body"`
}

// postSimulatedInboundSMS handles POST /v1/integrations/sms/{integrationId}/inbound:
// an operator's stand-in for a carrier delivering a text from a phone to a
// capture integration.
func postSimulatedInboundSMS(conn *connectivity.Service, sms *smsloop.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req simulateInboundRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.From == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "from is required")
			return
		}
		scope := identity.ScopeFrom(r.Context())
		// An environment flipped to production after the integration was made
		// still refuses: the capture sink must not take part in a live one.
		if err := conn.CheckCaptureAllowed(r.Context(), scope); err != nil {
			if errors.Is(err, connectivity.ErrCaptureInProduction) {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
				return
			}
			writeInternalError(w, r, err)
			return
		}
		res, err := sms.Simulate(r.Context(), scope, chi.URLParam(r, "integrationId"), req.From, req.To, req.Body)
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "capture integration not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"inboundMessageId": res.InboundMessageID,
			"keyword":          res.Keyword,
			"optChanged":       res.OptChanged,
		})
	}
}
