package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteJSONError_Envelope pins the stable error contract: a JSON
// Content-Type, the exact status passed in, and an
// {"error":{"code","message"}} body with the given code and message.
func TestWriteJSONError_Envelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusBadRequest, "bad_request", "bad input")

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var got errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not a valid JSON envelope: %v (body=%s)", err, rec.Body.String())
	}
	if got.Error.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", got.Error.Code)
	}
	if got.Error.Message != "bad input" {
		t.Errorf("message = %q, want \"bad input\"", got.Error.Message)
	}
}

// TestWriteInternalError_NoLeak proves the 5xx path never echoes the underlying
// error string to the client, and always emits the generic "internal" code.
func TestWriteInternalError_NoLeak(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)

	secret := "sql: secret table detail"
	writeInternalError(rec, req, errors.New(secret))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("5xx body leaked the internal error string: %q", body)
	}
	if !strings.Contains(body, "internal") {
		t.Fatalf("5xx body missing the generic \"internal\" code: %q", body)
	}
}
