package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/phoneverify"
)

// phoneConsentRequest is the body of POST
// /v1/subscribers/{subscriberId}/phone/consent.
type phoneConsentRequest struct {
	Phone string `json:"phone"`
	// Source is a free-text note on where consent came from. Optional.
	Source string `json:"source"`
	// AttestedAt is when consent was given, RFC 3339. Optional.
	AttestedAt string `json:"attestedAt"`
}

// phoneConsentDTO is the recorded attestation.
type phoneConsentDTO struct {
	ID           string `json:"id"`
	SubscriberID string `json:"subscriberId"`
	Phone        string `json:"phone"`
	PhoneConsent string `json:"phoneConsent"`
	APIKeyID     string `json:"apiKeyId"`
	Source       string `json:"source,omitempty"`
	AttestedAt   string `json:"attestedAt,omitempty"`
	PolicyURL    string `json:"policyUrl"`
	CreatedAt    string `json:"createdAt"`
}

// postPhoneConsent handles POST /v1/subscribers/{subscriberId}/phone/consent:
// the operator attests the subscriber consented to texts at phone. 201 with
// the attestation; 403 when the environment does not allow it or the key
// lacks the permission.
func postPhoneConsent(pv *phoneverify.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body phoneConsentRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "body must be JSON")
			return
		}
		key, _ := identity.KeyFrom(r.Context())
		a, err := pv.Attest(r.Context(), identity.ScopeFrom(r.Context()),
			phoneverify.Attester{KeyID: key.ID, MayAttest: key.AttestSMSConsent},
			phoneverify.AttestInput{
				SubscriberID: chi.URLParam(r, "subscriberId"),
				Phone:        body.Phone, Source: body.Source, AttestedAt: body.AttestedAt,
			})
		if err != nil {
			writePhoneConsentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, phoneConsentDTO{
			ID: a.ID, SubscriberID: a.SubscriberID, Phone: a.Phone,
			PhoneConsent: "operator_attested", APIKeyID: a.APIKeyID, Source: a.Source,
			AttestedAt: a.ConsentedAt, PolicyURL: a.PolicyURL, CreatedAt: a.CreatedAt,
		})
	}
}

// deletePhoneConsent handles DELETE
// /v1/subscribers/{subscriberId}/phone/consent: it revokes an operator
// attestation (204), keeping its audit row. A phone the subscriber verified
// themselves stays verified. Any API key of the environment may revoke.
func deletePhoneConsent(pv *phoneverify.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := pv.RevokeAttestation(r.Context(), identity.ScopeFrom(r.Context()), chi.URLParam(r, "subscriberId")); err != nil {
			writePhoneConsentError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// phoneConsentErrors maps each phoneverify outcome to its response.
var phoneConsentErrors = []struct {
	err    error
	status int
	code   string
}{
	{phoneverify.ErrAttestationDisabled, http.StatusForbidden, "sms_consent_disabled"},
	{phoneverify.ErrAttestNotPermitted, http.StatusForbidden, "forbidden"},
	{phoneverify.ErrUnknownSubscriber, http.StatusNotFound, "not_found"},
	{phoneverify.ErrInvalidPhone, http.StatusUnprocessableEntity, "invalid_phone"},
	{phoneverify.ErrInvalidAttestedAt, http.StatusBadRequest, "bad_request"},
	{phoneverify.ErrSourceTooLong, http.StatusBadRequest, "bad_request"},
}

func writePhoneConsentError(w http.ResponseWriter, r *http.Request, err error) {
	for _, e := range phoneConsentErrors {
		if errors.Is(err, e.err) {
			writeJSONError(w, e.status, e.code, strings.TrimPrefix(e.err.Error(), "phoneverify: "))
			return
		}
	}
	writeInternalError(w, r, err)
}
