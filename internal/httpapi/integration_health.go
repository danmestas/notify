package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/deliveryhealth"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// CredentialChecker runs an integration's credential check now and reports the
// verdict to delivery health before it returns. *credverify.Service satisfies
// it.
type CredentialChecker interface {
	CheckNow(ctx context.Context, scope domain.Scope, integrationID string) (connectivity.VerifyResult, error)
}

// integrationHealthDTO is the health object on GET /v1/integrations[/{id}] and
// the body of POST /v1/integrations/{id}/verify (#101). It carries only what
// delivery health records: the state, when it began, and the latest counted
// error as the provider reported it. It never carries a credential.
type integrationHealthDTO struct {
	// State is healthy, degraded, failing or unverified.
	State string `json:"state"`
	// Since is when the integration entered State (RFC 3339); absent for an
	// integration nothing has been recorded for yet.
	Since            string `json:"since,omitempty"`
	LastErrorClass   string `json:"lastErrorClass,omitempty"`
	LastErrorMessage string `json:"lastErrorMessage,omitempty"`
	// LastCheckedAt is when the credentials were last checked, if ever.
	LastCheckedAt string `json:"lastCheckedAt,omitempty"`
	// LastCheckOK says whether that check passed; absent if none ran. A
	// failed check that was not a rejected credential (an unreachable server)
	// leaves State as it was, so this and LastCheckDetail say what happened.
	LastCheckOK     *bool  `json:"lastCheckOk,omitempty"`
	LastCheckDetail string `json:"lastCheckDetail,omitempty"`
}

// toHealthDTO maps a stored health row; found=false (no row yet) is
// unverified.
func toHealthDTO(h domain.IntegrationHealth, found bool) integrationHealthDTO {
	if !found {
		return integrationHealthDTO{State: string(deliveryhealth.Unverified)}
	}
	dto := integrationHealthDTO{
		State:            h.State,
		Since:            h.Since,
		LastErrorClass:   h.LastErrorClass,
		LastErrorMessage: h.LastErrorMessage,
		LastCheckedAt:    h.LastCheckAt,
		LastCheckDetail:  h.LastCheckDetail,
	}
	if h.LastCheckAt != "" {
		ok := h.LastCheckOK
		dto.LastCheckOK = &ok
	}
	return dto
}

// integrationHealth loads one integration's health in scope.
func integrationHealth(ctx context.Context, st *store.Store, scope domain.Scope, id string) (integrationHealthDTO, error) {
	h, err := st.IntegrationHealth().Get(ctx, scope, id)
	if errors.Is(err, store.ErrNotFound) {
		return toHealthDTO(domain.IntegrationHealth{}, false), nil
	}
	if err != nil {
		return integrationHealthDTO{}, err
	}
	return toHealthDTO(h, true), nil
}

// postIntegrationVerify handles POST /v1/integrations/{id}/verify: run the
// credential check now and return the integration's resulting health. A
// provider with no credential probe leaves health as it was. 404 for an
// integration outside the caller's scope.
func postIntegrationVerify(conn *connectivity.Service, chk CredentialChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())
		st := conn.Store()
		if _, err := st.Integrations().GetByID(r.Context(), scope, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "integration not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		if _, err := chk.CheckNow(r.Context(), scope, id); err != nil {
			// The probe could not complete (undecryptable or malformed
			// credentials); the detail stays in the log, not the response.
			slog.ErrorContext(r.Context(), "httpapi: integration verify", "id", id, "err", err)
			writeJSONError(w, http.StatusBadGateway, "verify_failed", "the credential check could not be completed")
			return
		}
		h, err := integrationHealth(r.Context(), st, scope, id)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, h)
	}
}
