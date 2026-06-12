// auth_test.go exercises the console session lifecycle end-to-end against
// the real identity service + temp SQLite store: redirect-when-anonymous,
// login with a bad then a good key, cookie attributes, tamper rejection,
// and logout.
//
// Methodology:
//   - No mocks: every flow goes through identity.Authenticate against the
//     bootstrap tenant (repo convention).
//   - Minimum 2 assertions per test (positive + negative space).
package console

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuth_UnauthenticatedRedirectsToLogin(t *testing.T) {
	tc := newTestConsole(t)
	for _, path := range []string{"/console", "/console/", "/console/api/fragments/dashboard-kpis"} {
		rr := tc.get(t, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Errorf("GET %s: status = %d, want 303", path, rr.Code)
		}
		if got := rr.Header().Get("Location"); got != "/console/login" {
			t.Errorf("GET %s: Location = %q, want /console/login", path, got)
		}
	}
}

func TestAuth_LoginPageRenders(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/login", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/login: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/console/login"`) {
		t.Errorf("login page missing form action")
	}
	if !strings.Contains(body, `name="api_key"`) {
		t.Errorf("login page missing api_key input")
	}
}

func TestAuth_LoginBadKeyRerendersWithError(t *testing.T) {
	tc := newTestConsole(t)
	form := strings.NewReader("api_key=deadbeef")
	req := httptest.NewRequest(http.MethodPost, "/console/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("bad key: status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not recognised") {
		t.Errorf("bad key: error line missing from re-rendered form")
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Errorf("bad key: cookie was set: %v", rr.Result().Cookies())
	}
}

func TestAuth_LoginGoodKeySetsCookieAndServesDashboard(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	if !cookie.HttpOnly {
		t.Errorf("session cookie not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/console" {
		t.Errorf("session cookie Path = %q, want /console", cookie.Path)
	}

	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/ with session: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Dashboard") {
		t.Errorf("dashboard body missing title")
	}
}

func TestAuth_AuthenticatedLoginPageBouncesToDashboard(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/login", cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("GET /console/login with session: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/" {
		t.Errorf("Location = %q, want /console/", got)
	}
}

func TestAuth_TamperedCookieRejected(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	// Flip one character of the sealed payload; GCM must refuse it.
	raw := []rune(cookie.Value)
	mid := len(raw) / 2
	if raw[mid] == 'A' {
		raw[mid] = 'B'
	} else {
		raw[mid] = 'A'
	}
	tampered := &http.Cookie{Name: cookie.Name, Value: string(raw)}
	rr := tc.get(t, "/console/", tampered)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("tampered cookie: status = %d, want 303 redirect", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("tampered cookie: Location = %q, want /console/login", got)
	}
}

func TestAuth_LogoutClearsCookieAndRedirects(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)

	form := strings.NewReader("csrf_token=" + csrf)
	req := httptest.NewRequest(http.MethodPost, "/console/logout", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("logout: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("logout: Location = %q, want /console/login", got)
	}
	var cleared *http.Cookie
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == sessionCookieName {
			cleared = ck
		}
	}
	if cleared == nil {
		t.Fatalf("logout: no Set-Cookie for %s", sessionCookieName)
	}
	if cleared.MaxAge >= 0 {
		t.Errorf("logout: cookie MaxAge = %d, want negative (delete)", cleared.MaxAge)
	}
	if cleared.Value != "" {
		t.Errorf("logout: cookie value = %q, want empty", cleared.Value)
	}
}

func TestAuth_RevokedKeyKillsSession(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// Simulate revocation: wipe the api_keys table underneath the live
	// session. The next request must re-authenticate and fail.
	tc.revokeAllKeys(t)
	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("revoked key: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("revoked key: Location = %q, want /console/login", got)
	}
}

func TestAuth_SecureFlagFollowsTLS(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	if cookie.Secure {
		t.Errorf("plain-HTTP login set Secure — localhost dev would lose the cookie")
	}

	// Same login behind a TLS-terminating proxy: Secure must be set on
	// both the session cookie and the logout clear.
	form := strings.NewReader("api_key=" + tc.key)
	req := httptest.NewRequest(http.MethodPost, "/console/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("proxied login: status = %d, want 303", rr.Code)
	}
	var secureCookie *http.Cookie
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == sessionCookieName {
			secureCookie = ck
		}
	}
	if secureCookie == nil {
		t.Fatalf("proxied login: no session cookie")
	}
	if !secureCookie.Secure {
		t.Errorf("proxied (X-Forwarded-Proto: https) login cookie not Secure")
	}

	s, ok := tc.c.openSession(secureCookie.Value)
	if !ok {
		t.Fatalf("proxied session did not open")
	}
	logout := httptest.NewRequest(http.MethodPost, "/console/logout",
		strings.NewReader("csrf_token="+s.CSRF))
	logout.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logout.Header.Set("X-Forwarded-Proto", "https")
	logout.AddCookie(secureCookie)
	lr := httptest.NewRecorder()
	tc.h.ServeHTTP(lr, logout)
	var cleared *http.Cookie
	for _, ck := range lr.Result().Cookies() {
		if ck.Name == sessionCookieName {
			cleared = ck
		}
	}
	if cleared == nil {
		t.Fatalf("proxied logout: no clear cookie")
	}
	if !cleared.Secure {
		t.Errorf("proxied logout clear-cookie not Secure — browser would keep the original")
	}
}

func TestAuth_ForeignAADBlobRejected(t *testing.T) {
	// Domain separation: a blob sealed under the same master key but
	// without the console's AAD tag — identity's at-rest encryptions use
	// gcm.Seal(nonce, nonce, plaintext, nil) — must never open as a
	// session, even when its plaintext happens to be a valid session
	// JSON.
	tc := newTestConsole(t)
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	plain, err := json.Marshal(session{
		Key:  tc.key,
		CSRF: "deadbeef",
		Exp:  time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	foreign := base64.RawURLEncoding.EncodeToString(
		gcm.Seal(nonce, nonce, plain, nil),
	) // nil AAD = identity's domain

	if _, ok := tc.c.openSession(foreign); ok {
		t.Fatalf("nil-AAD blob opened as a session — domain separation broken")
	}
	rr := tc.get(t, "/console/", &http.Cookie{Name: sessionCookieName, Value: foreign})
	if rr.Code != http.StatusSeeOther {
		t.Errorf("foreign blob cookie: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("foreign blob cookie: Location = %q, want /console/login", got)
	}
}
