package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorEnvelope is the stable JSON error shape returned by every /v1 handler:
//
//	{"error":{"code":"...","message":"..."}}
//
// code is a short, stable, machine-readable token (bad_request, not_found,
// conflict, unprocessable_entity, internal); message is human-readable and,
// for 5xx, deliberately generic so no internal detail reaches the client.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSONError writes the stable {"error":{"code","message"}} envelope with
// the given HTTP status. It is the error-path counterpart to writeJSON: every
// /v1 handler uses it (or writeInternalError) instead of http.Error so clients
// see one JSON contract rather than ad-hoc plain-text bodies.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: msg}})
}

// writeInternalError is the single 5xx exit for /v1 handlers. It logs the real
// error (with request method and path for correlation) and emits a generic
// envelope, so an internal error string is never leaked to the client. This
// pulls the leak-vs-log decision out of every call site into one place.
func writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(
		r.Context(), "httpapi: internal error",
		"method", r.Method,
		"path", r.URL.Path,
		"err", err,
	)
	writeJSONError(w, http.StatusInternalServerError, "internal", "internal error")
}
