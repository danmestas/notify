package console

import (
	"crypto/subtle"
	"net/http"
)

// csrf.go owns the console's CSRF protection (dagnats csrf.go port,
// adapted to cookie sessions): every state-changing request under
// /console/* must echo the per-session token minted at login. The token
// lives inside the encrypted session cookie, so "knows the token" implies
// "can read the operator's page" — exactly the bar CSRF raises. Same-
// origin policy plus SameSite=Lax already block the obvious attack; this
// is defence-in-depth.
//
// Token sources, checked in order:
//   - X-CSRF-Token request header (JS-driven requests).
//   - csrf_token form field (traditional HTML forms).
//
// Missing or invalid tokens get a 403 + a JSON body naming the header.
// GET / HEAD / OPTIONS pass through; the policy is mutation-only.

// csrfMiddleware enforces the token on non-GET requests. It runs inside
// sessionMiddleware, so the session is always on the context here.
func (c *Console) csrfMiddleware(next http.Handler) http.Handler {
	if next == nil {
		panic("csrfMiddleware: next is nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCSRFExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !csrfTokenValid(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(csrfDeniedBody))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isCSRFExempt returns true for read-only methods. Mutations only.
func isCSRFExempt(r *http.Request) bool {
	if r == nil {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// csrfTokenValid extracts the presented token and compares it against the
// session's in constant time. sessionMiddleware guarantees the session is
// on the context; a missing one fails closed.
func csrfTokenValid(r *http.Request) bool {
	if r == nil {
		return false
	}
	s, ok := sessionFromContext(r.Context())
	if !ok || s.CSRF == "" {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		token = r.FormValue("csrf_token")
	}
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRF)) == 1
}

// csrfDeniedBody is the verbatim JSON written on a CSRF failure. Names
// both transports so the client knows exactly what to send.
const csrfDeniedBody = `{"error":"csrf_invalid","message":` +
	`"CSRF token missing or invalid. Submit the token via the ` +
	`X-CSRF-Token header or the csrf_token form field."}`
