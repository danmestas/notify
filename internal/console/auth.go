package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// auth.go owns the console's operator session: the login page where the
// operator pastes an API key, the encrypted session cookie that key lives
// in afterwards, the middleware that re-resolves the key to a tenancy
// Scope on every request, and logout.
//
// Design (anchor decision 4 + dagnats auth.go idiom, adapted to notify's
// ApiKey guard): the cookie payload is the raw API key plus a per-session
// CSRF token and an expiry, sealed with AES-256-GCM under the same master
// key identity uses. Storing the key — not a resolved Scope — means every
// request goes back through identity.Authenticate, so revoking the API
// key kills its console sessions immediately and Scope resolution stays
// in one place.

// sessionCookieName is the operator-session cookie. Path-scoped to
// /console so the cookie never rides along on /v1/* API calls.
const sessionCookieName = "notify_console_session"

// sessionTTL bounds how long a pasted key stays signed in. One week is
// the operator-friendly default; re-pasting the key is cheap.
const sessionTTL = 7 * 24 * time.Hour

// sessionAAD is the additional-authenticated-data tag sealed into every
// session cookie. It domain-separates console sessions from every other
// AES-GCM blob sealed under the same master key (identity's at-rest key
// encryptions, connectivity's credentials): a ciphertext minted by one
// domain can never be replayed as another's, because Open fails unless
// the AAD matches. Bump the suffix if the payload shape ever changes
// incompatibly.
const sessionAAD = "notify-console-session-v1"

// session is the decrypted cookie payload.
type session struct {
	// Key is the raw hex API key the operator pasted; re-authenticated
	// on every request.
	Key string `json:"key"`
	// CSRF is the per-session token non-GET requests must echo.
	CSRF string `json:"csrf"`
	// Exp is the unix-seconds expiry; past it the cookie is dead even
	// though the GCM seal still verifies.
	Exp int64 `json:"exp"`
}

// sessionCtxKey is unexported so callers must use the helpers below.
type sessionCtxKey struct{}

// sealSession encrypts s into the cookie value: AES-256-GCM with the
// random nonce prepended, base64url-encoded.
func (c *Console) sealSession(s session) (string, error) {
	if c == nil {
		panic("sealSession: c is nil")
	}
	if s.Key == "" {
		panic("sealSession: empty key")
	}
	plain, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("console: marshal session: %w", err)
	}
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("console: session nonce: %w", err)
	}
	sealed := c.gcm.Seal(nonce, nonce, plain, []byte(sessionAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openSession reverses sealSession and checks the expiry. Any failure —
// bad base64, tampered ciphertext, malformed JSON, expired — returns
// ok=false; the caller treats every failure identically (redirect to
// login) so the error detail never reaches the browser.
func (c *Console) openSession(value string) (session, bool) {
	if c == nil {
		panic("openSession: c is nil")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return session{}, false
	}
	ns := c.gcm.NonceSize()
	if len(sealed) < ns {
		return session{}, false
	}
	plain, err := c.gcm.Open(nil, sealed[:ns], sealed[ns:], []byte(sessionAAD))
	if err != nil {
		return session{}, false
	}
	var s session
	if err := json.Unmarshal(plain, &s); err != nil {
		return session{}, false
	}
	if s.Key == "" || s.CSRF == "" || time.Now().Unix() >= s.Exp {
		return session{}, false
	}
	return s, true
}

// sessionFrom extracts and opens the session cookie on r. ok=false on a
// missing or invalid cookie.
func (c *Console) sessionFrom(r *http.Request) (session, bool) {
	if r == nil {
		panic("sessionFrom: r is nil")
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return session{}, false
	}
	return c.openSession(cookie.Value)
}

// sessionFromContext returns the session the middleware attached. The
// boolean reports whether one is present (it always is behind
// sessionMiddleware; the 404 handler also probes it directly).
func sessionFromContext(ctx context.Context) (session, bool) {
	if ctx == nil {
		panic("sessionFromContext: ctx is nil")
	}
	s, ok := ctx.Value(sessionCtxKey{}).(session)
	return s, ok
}

// sessionMiddleware guards everything under /console except login and
// assets: it opens the cookie, re-authenticates the API key, and attaches
// both the resolved Scope (via domain.ContextWithScope, the same contract
// httpapi handlers rely on) and the session to the request context. Any
// failure redirects to the login page — never a bare 401, the operator is
// a browser.
func (c *Console) sessionMiddleware(next http.Handler) http.Handler {
	if next == nil {
		panic("sessionMiddleware: next is nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := c.sessionFrom(r)
		if !ok {
			redirectToLogin(w, r)
			return
		}
		scope, err := c.deps.Identity.Authenticate(r.Context(), s.Key)
		if err != nil {
			// Key revoked (or store unreachable). Clear the dead cookie so
			// the browser stops re-presenting it, then ask for a fresh key.
			clearSessionCookie(w, r)
			redirectToLogin(w, r)
			return
		}
		ctx := domain.ContextWithScope(r.Context(), scope)
		ctx = context.WithValue(ctx, sessionCtxKey{}, s)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// redirectToLogin sends the browser to the login page. 303 forces the
// follow-up to be a GET even when the guarded request was a mutation.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("redirectToLogin: w is nil")
	}
	if r == nil {
		panic("redirectToLogin: r is nil")
	}
	http.Redirect(w, r, "/console/login", http.StatusSeeOther)
}

// loginView powers the login page template.
type loginView struct {
	// Error is the operator-facing failure line; empty on first render.
	Error string
	// CSRF is the pre-session double-submit token embedded in the form. It is
	// minted per render and must equal the value in the login cookie for the
	// POST to be accepted; see mintLoginCSRF.
	CSRF string
}

// loginCSRFCookieName holds the pre-session (double-submit) CSRF token.
//
// The login POST is the one mutation that cannot carry a session token: no
// session exists yet. SameSite does not help either -- the attack lands before
// any cookie exists, so there is nothing for the browser to withhold. Without
// this, an attacker can auto-submit THEIR api key to this endpoint and silently
// sign the operator into the attacker's tenant scope, where everything the
// operator then reads or triggers is attacker-controlled.
//
// The construction: mint one random value, hand it out as both a path-scoped
// cookie and a hidden form field, and require them to match on POST. A
// cross-site page can cause the form field to be sent, but same-origin policy
// stops it reading or forging the cookie, so the pair cannot match.
const loginCSRFCookieName = "notify_console_login_csrf"

// loginCSRFTTL bounds how long a rendered login form stays submittable. Long
// enough that a distracted operator is not surprised, short enough that a token
// captured from a stale page is not useful.
const loginCSRFTTL = 15 * time.Minute

// loginCSRFTokenLen is the hex length of a minted token (32 random bytes).
const loginCSRFTokenLen = 64

// mintLoginCSRF returns the token to embed in the form and ensures the matching
// cookie is set.
//
// It REUSES a well-formed token the request already carries rather than always
// minting a new one. Always minting looks tighter but is broken in practice: a
// browser loads the login page more than once (reload, back button, a prefetch,
// a second tab), and each render would overwrite the cookie while an earlier
// rendered form still held the previous value -- so the form the operator
// actually submits fails to match, intermittently and unreproducibly.
//
// Reuse costs nothing here. The token's job is to prove the request came from a
// form this origin served to this browser, which rests on the attacker being
// unable to READ the cookie (HttpOnly + same-origin policy). That property is
// unchanged by serving the same value twice within its short lifetime.
//
// The length/charset check keeps a garbage or attacker-planted cookie value from
// being adopted as the token: anything not shaped like a minted token is
// replaced rather than trusted.
func mintLoginCSRF(w http.ResponseWriter, r *http.Request) (string, error) {
	if existing, err := r.Cookie(loginCSRFCookieName); err == nil && isMintedLoginToken(existing.Value) {
		// Re-set it so a long-lived tab keeps a live cookie rather than
		// silently ageing out mid-form.
		setLoginCSRFCookie(w, r, existing.Value)
		return existing.Value, nil
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	setLoginCSRFCookie(w, r, token)
	return token, nil
}

// isMintedLoginToken reports whether v has the shape this package mints.
func isMintedLoginToken(v string) bool {
	if len(v) != loginCSRFTokenLen {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// setLoginCSRFCookie writes the pre-session cookie. Scoped to the login path:
// it has no business travelling with authenticated console requests.
func setLoginCSRFCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookieName,
		Value:    token,
		Path:     "/console/login",
		MaxAge:   int(loginCSRFTTL / time.Second),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearLoginCSRF expires the pre-session cookie. Called once a real session
// exists so the login token cannot linger or be mistaken for the session's own
// CSRF token.
func clearLoginCSRF(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookieName,
		Value:    "",
		Path:     "/console/login",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// loginCSRFValid reports whether the request carries a matching cookie/field
// pair. Fails closed on any missing part, and compares in constant time.
func loginCSRFValid(r *http.Request) bool {
	if r == nil {
		return false
	}
	ck, err := r.Cookie(loginCSRFCookieName)
	if err != nil || ck.Value == "" {
		return false
	}
	field := r.PostFormValue("csrf_token")
	if field == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(field), []byte(ck.Value)) == 1
}

// serveLoginPage renders the paste-your-key form. An already
// authenticated operator is bounced straight to the dashboard.
func (c *Console) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.sessionFrom(r); ok {
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return
	}
	c.renderLogin(w, r, http.StatusOK, loginView{})
}

// serveLoginSubmit validates the pasted key via identity.Authenticate and
// mints the session cookie. A bad key re-renders the form with an error
// and no cookie; the response is 401 so probes can't mistake the
// re-rendered form for success.
func (c *Console) serveLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.renderLogin(w, r, http.StatusBadRequest, loginView{Error: "Malformed form submission."})
		return
	}
	// Checked before the credential is touched: if a rejected token produced a
	// different response for a valid key than for a garbage one, this endpoint
	// would be an oracle for testing stolen keys. The operator-facing text is
	// deliberately the same generic failure used for a bad key.
	if !loginCSRFValid(r) {
		c.renderLogin(w, r, http.StatusUnauthorized, loginView{Error: "That API key was not recognised."})
		return
	}
	key := r.PostFormValue("api_key")
	if key == "" {
		c.renderLogin(w, r, http.StatusUnauthorized, loginView{Error: "Paste an API key to sign in."})
		return
	}
	if _, err := c.deps.Identity.Authenticate(r.Context(), key); err != nil {
		c.renderLogin(w, r, http.StatusUnauthorized, loginView{Error: "That API key was not recognised."})
		return
	}
	csrf := make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		c.deps.Logger.Error("console: mint csrf token", "err", err)
		c.serveError(w, r)
		return
	}
	value, err := c.sealSession(session{
		Key:  key,
		CSRF: hex.EncodeToString(csrf),
		Exp:  time.Now().Add(sessionTTL).Unix(),
	})
	if err != nil {
		c.deps.Logger.Error("console: seal session", "err", err)
		c.serveError(w, r)
		return
	}
	setSessionCookie(w, r, value)
	// The pre-session token has done its job; a real session CSRF token now
	// exists. Leaving it set would keep a second, weaker token in play.
	clearLoginCSRF(w, r)
	http.Redirect(w, r, "/console/", http.StatusSeeOther)
}

// setSessionCookie sets the sealed session cookie value on the response.
// Shared by the login flow and by handleAPIKeyRegenerate — rotating the
// operator's own active key must re-seal the cookie with the new key in the
// same response, or the very next request would bounce the operator to
// login (the old key's row is gone) before the reveal card ever renders.
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	if w == nil {
		panic("setSessionCookie: w is nil")
	}
	if r == nil {
		panic("setSessionCookie: r is nil")
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/console",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// serveLogout clears the session cookie and returns to login. Mounted
// behind sessionMiddleware + csrfMiddleware, so only the signed-in
// operator's own page can trigger it.
func (c *Console) serveLogout(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveLogout: w is nil")
	}
	if r == nil {
		panic("serveLogout: r is nil")
	}
	clearSessionCookie(w, r)
	redirectToLogin(w, r)
}

// requestIsTLS reports whether the request arrived over TLS, either
// directly or via a TLS-terminating proxy/tunnel announcing itself with
// X-Forwarded-Proto. Drives the cookie Secure flag: hardened behind
// TLS, still functional on plain-HTTP localhost dev.
func requestIsTLS(r *http.Request) bool {
	if r == nil {
		panic("requestIsTLS: r is nil")
	}
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// clearSessionCookie expires the session cookie on the browser. The
// attribute set must mirror the login path's (including Secure) so the
// browser treats it as the same cookie and actually drops it.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("clearSessionCookie: w is nil")
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/console",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// renderLogin executes the standalone login template (the login page does
// not wear the console chrome — there is nothing to navigate to yet).
func (c *Console) renderLogin(w http.ResponseWriter, r *http.Request, status int, view loginView) {
	if w == nil {
		panic("renderLogin: w is nil")
	}
	if r == nil {
		panic("renderLogin: r is nil")
	}
	// Mint a fresh pre-session token on EVERY render, including error
	// re-renders: a re-rendered form carrying a spent token would fail the
	// operator's retry for a reason that looks nothing like the real cause.
	token, err := mintLoginCSRF(w, r)
	if err != nil {
		c.deps.Logger.Error("console: mint login csrf", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	view.CSRF = token
	html, err := renderFragment(c.ts.base, "login", view)
	if err != nil {
		c.deps.Logger.Error("console: render login", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}
