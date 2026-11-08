package inboxapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
)

// WithPhoneVerification mounts the subscriber's phone verification routes
// (#130) and returns svc. Without it the routes are absent.
func (svc *Service) WithPhoneVerification(pv *phoneverify.Service) *Service {
	svc.phone = pv
	return svc
}

// phoneRoutes registers POST /v1/me/phone/verification (send a code) and
// POST /v1/me/phone/verification/confirm (check it). "me" is the subscriber
// the session token names; nothing in the request can name another.
func (svc *Service) phoneRoutes(r chi.Router) {
	r.Post("/v1/me/phone/verification", svc.requestPhoneCode)
	r.Post("/v1/me/phone/verification/confirm", svc.confirmPhoneCode)
}

// sessionSubscriber is the subscriber a session pins. A phone is verified by
// its subscriber only, so a tenant API key, which can act for anyone, is
// refused.
func sessionSubscriber(w http.ResponseWriter, r *http.Request) (domain.Scope, string, bool) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return domain.Scope{}, "", false
	}
	sub, pinned := domain.PinnedSubscriberFromContext(r.Context())
	if !pinned {
		writeError(w, http.StatusForbidden, "subscriber_session_required",
			"only the subscriber can verify their phone: use a subscriber session token")
		return domain.Scope{}, "", false
	}
	return scope, sub, true
}

func (svc *Service) requestPhoneCode(w http.ResponseWriter, r *http.Request) {
	scope, sub, ok := sessionSubscriber(w, r)
	if !ok {
		return
	}
	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be {\"phone\":\"+15551234567\"}")
		return
	}
	pending, err := svc.phone.Request(r.Context(), scope, sub, body.Phone)
	if err != nil {
		writePhoneError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"phone":     pending.Phone,
		"expiresAt": pending.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// confirmPhoneCode verifies the number the code was sent to and makes it the
// subscriber's phone, replacing any phone the sender wrote since the request:
// holding the code proves the subscriber holds that number.
func (svc *Service) confirmPhoneCode(w http.ResponseWriter, r *http.Request) {
	scope, sub, ok := sessionSubscriber(w, r)
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be {\"code\":\"123456\"}")
		return
	}
	verified, err := svc.phone.Confirm(r.Context(), scope, sub, body.Code)
	if err != nil {
		writePhoneError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"phone":      verified.Phone,
		"verifiedAt": verified.VerifiedAt.UTC().Format(time.RFC3339),
	})
}

// phoneErrors maps each phoneverify outcome to its response.
var phoneErrors = []struct {
	err    error
	status int
	code   string
}{
	{phoneverify.ErrInvalidPhone, http.StatusUnprocessableEntity, "invalid_phone"},
	{phoneverify.ErrUnknownSubscriber, http.StatusNotFound, "not_found"},
	{phoneverify.ErrRateLimited, http.StatusTooManyRequests, "rate_limited"},
	{phoneverify.ErrDailyLimit, http.StatusTooManyRequests, "daily_limit"},
	{phoneverify.ErrEnvironmentDailyLimit, http.StatusTooManyRequests, "environment_daily_limit"},
	{phoneverify.ErrSendFailed, http.StatusBadGateway, "sms_send_failed"},
	{phoneverify.ErrNoPendingCode, http.StatusConflict, "no_pending_code"},
	{phoneverify.ErrCodeExpired, http.StatusGone, "code_expired"},
	{phoneverify.ErrWrongCode, http.StatusUnprocessableEntity, "wrong_code"},
	{phoneverify.ErrTooManyAttempts, http.StatusTooManyRequests, "too_many_attempts"},
}

func writePhoneError(w http.ResponseWriter, r *http.Request, err error) {
	for _, e := range phoneErrors {
		if errors.Is(err, e.err) {
			if e.err == phoneverify.ErrSendFailed {
				slog.WarnContext(r.Context(), "inboxapi: verification code not sent", "err", err)
			}
			// The sentinel's plain text, never the wrapped provider error.
			writeError(w, e.status, e.code, strings.TrimPrefix(e.err.Error(), "phoneverify: "))
			return
		}
	}
	slog.ErrorContext(r.Context(), "inboxapi: phone verification", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// writeError writes the /v1 error envelope {"error":{"code","message"}}.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
