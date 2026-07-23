package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The login POST is the one mutation that cannot carry a session CSRF token --
// no session exists yet, which is also why SameSite gives no protection here:
// the attack lands before any cookie exists, so there is nothing for the browser
// to withhold. Without a pre-session token an attacker can auto-submit THEIR api
// key to this endpoint and silently sign the operator into the attacker's tenant
// scope, where everything the operator subsequently reads or triggers is
// attacker-controlled.
//
// These tests pin the double-submit construction: one random value handed out as
// both a path-scoped cookie and a hidden form field, required to match on POST.

// loginPage renders the form and returns the embedded token plus the cookies a
// browser would carry forward.
func loginPage(t *testing.T, tc *testConsole) (token string, jar []*http.Cookie) {
	t.Helper()
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/console/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("login page status = %d, want 200", rr.Code)
	}
	return loginCSRFTokenFrom(t, rr.Body.String()), rr.Result().Cookies()
}

// submitLogin posts the form with the supplied key/token/cookies.
func submitLogin(tc *testConsole, key, token string, jar []*http.Cookie) *httptest.ResponseRecorder {
	form := url.Values{"api_key": {key}}
	if token != "" {
		form.Set("csrf_token", token)
	}
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, ck := range jar {
		req.AddCookie(ck)
	}
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	return rr
}

// sessionEstablished reports whether a response signed the operator in.
func sessionEstablished(rr *httptest.ResponseRecorder) bool {
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == sessionCookieName && ck.Value != "" && ck.MaxAge >= 0 {
			return true
		}
	}
	return false
}

// TestLoginCSRF_NoTokenRejected is the attack itself: a cross-site form post
// carries no pre-session cookie and no field, so it must not sign anyone in --
// however good the pasted key is.
func TestLoginCSRF_NoTokenRejected(t *testing.T) {
	tc := newTestConsole(t)

	rr := submitLogin(tc, tc.key, "", nil)

	if sessionEstablished(rr) {
		t.Fatal("login without a pre-session CSRF token established a session")
	}
	if rr.Code == http.StatusSeeOther {
		t.Fatalf("login without a token returned 303: it was treated as success")
	}
}

// TestLoginCSRF_MismatchedTokenRejected covers an attacker who can put a value
// in the field but cannot set the cookie: the two must actually be compared.
func TestLoginCSRF_MismatchedTokenRejected(t *testing.T) {
	tc := newTestConsole(t)
	_, jar := loginPage(t, tc)

	rr := submitLogin(tc, tc.key, "not-the-minted-token", jar)

	if sessionEstablished(rr) {
		t.Fatal("login with a mismatched CSRF token established a session")
	}
}

// TestLoginCSRF_CookieAloneIsNotEnough pins the other half of double-submit: the
// cookie travels automatically on a cross-site post, so it cannot be sufficient
// on its own -- the attacker must also produce the field, which they cannot read.
func TestLoginCSRF_CookieAloneIsNotEnough(t *testing.T) {
	tc := newTestConsole(t)
	_, jar := loginPage(t, tc)

	rr := submitLogin(tc, tc.key, "", jar)

	if sessionEstablished(rr) {
		t.Fatal("login with only the cookie (no form field) established a session")
	}
}

// TestLoginCSRF_RejectionDoesNotLeakKeyValidity pins that the token is checked
// BEFORE the credential. If a rejected token answered differently for a valid
// key than a garbage one, this endpoint would be an oracle for testing stolen
// keys against the tenant.
func TestLoginCSRF_RejectionDoesNotLeakKeyValidity(t *testing.T) {
	tc := newTestConsole(t)

	valid := submitLogin(tc, tc.key, "", nil)
	garbage := submitLogin(tc, "definitely-not-a-real-key", "", nil)

	if valid.Code != garbage.Code {
		t.Errorf("status differs by key validity: valid=%d garbage=%d", valid.Code, garbage.Code)
	}
	if stripCSRF(valid.Body.String()) != stripCSRF(garbage.Body.String()) {
		t.Error("body differs by key validity: the endpoint is a key-validity oracle")
	}
}

// stripCSRF removes the per-render token so two responses can be compared for
// meaningful difference rather than randomness.
func stripCSRF(body string) string {
	i := strings.Index(body, `name="csrf_token"`)
	if i < 0 {
		return body
	}
	start := strings.LastIndex(body[:i], "<input")
	end := strings.Index(body[start:], ">")
	if start < 0 || end < 0 {
		return body
	}
	return body[:start] + body[start+end:]
}

// TestLoginCSRF_HappyPathStillWorks guards against closing the hole by breaking
// login outright.
func TestLoginCSRF_HappyPathStillWorks(t *testing.T) {
	tc := newTestConsole(t)
	token, jar := loginPage(t, tc)

	rr := submitLogin(tc, tc.key, token, jar)

	if !sessionEstablished(rr) {
		t.Fatalf("valid login with a good token did not sign in (status %d)", rr.Code)
	}
}

// TestLoginCSRF_FailedAttemptReRendersUsableToken covers the ordinary operator
// path -- mistype the key, then get it right. A re-render carrying a spent token
// would fail the retry for a reason that looks nothing like the real cause.
func TestLoginCSRF_FailedAttemptReRendersUsableToken(t *testing.T) {
	tc := newTestConsole(t)
	token, jar := loginPage(t, tc)

	first := submitLogin(tc, "wrong-key", token, jar)
	if sessionEstablished(first) {
		t.Fatal("a wrong key established a session")
	}

	retryJar := first.Result().Cookies()
	if len(retryJar) == 0 {
		retryJar = jar
	}
	retryToken := loginCSRFTokenFrom(t, first.Body.String())

	second := submitLogin(tc, tc.key, retryToken, retryJar)
	if !sessionEstablished(second) {
		t.Fatalf("retry after a failed attempt did not sign in (status %d)", second.Code)
	}
}

// TestLoginCSRF_PreSessionCookieClearedOnSuccess keeps the login-only token from
// outliving the flow, where it would sit alongside the session's own CSRF token
// as a second, weaker credential.
func TestLoginCSRF_PreSessionCookieClearedOnSuccess(t *testing.T) {
	tc := newTestConsole(t)
	token, jar := loginPage(t, tc)

	rr := submitLogin(tc, tc.key, token, jar)
	if !sessionEstablished(rr) {
		t.Fatalf("precondition failed: login did not succeed (status %d)", rr.Code)
	}

	for _, ck := range rr.Result().Cookies() {
		if ck.Name == loginCSRFCookieName {
			if ck.MaxAge < 0 || ck.Value == "" {
				return // explicitly expired
			}
			t.Fatalf("pre-session cookie survived login (value=%q maxage=%d)", ck.Value, ck.MaxAge)
		}
	}
	t.Fatalf("successful login did not clear %q", loginCSRFCookieName)
}

// TestLoginCSRF_TokenIsPerBrowser proves a browser arriving with no cookie gets
// its own random token, so one operator's token is useless in another's browser.
func TestLoginCSRF_TokenIsPerBrowser(t *testing.T) {
	tc := newTestConsole(t)

	a, _ := loginPage(t, tc)
	b, _ := loginPage(t, tc)

	if a == b {
		t.Fatal("two cookie-less renders produced the same token: it is not random per browser")
	}
}

// TestLoginCSRF_TokenStableAcrossReRenders is the regression test for a real
// break found by the browser smoke test.
//
// Browsers load the login page more than once -- reload, back button, prefetch,
// a second tab. When every render minted a fresh token, a later render
// overwrote the cookie while an earlier rendered form still held the previous
// value, so the form the operator actually submitted no longer matched. Login
// failed intermittently and unreproducibly, with a "key not recognised" message
// that pointed nowhere near the cause.
//
// A render that already carries a valid token must therefore keep it.
func TestLoginCSRF_TokenStableAcrossReRenders(t *testing.T) {
	tc := newTestConsole(t)

	first, jar := loginPage(t, tc)

	// Same browser loads the page again, carrying its cookie.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/login", nil)
	for _, ck := range jar {
		req.AddCookie(ck)
	}
	tc.h.ServeHTTP(rr, req)
	second := loginCSRFTokenFrom(t, rr.Body.String())

	if second != first {
		t.Fatalf("re-render rotated the token (%q -> %q): a form rendered before a "+
			"reload would no longer submit", first, second)
	}

	// And the form rendered first must still work after the second render.
	if !sessionEstablished(submitLogin(tc, tc.key, first, jar)) {
		t.Fatal("the originally rendered form stopped working after a re-render")
	}
}

// TestLoginCSRF_GarbageCookieNotAdopted keeps a planted or corrupt cookie value
// from being reused as the token: only well-formed minted values are trusted.
func TestLoginCSRF_GarbageCookieNotAdopted(t *testing.T) {
	tc := newTestConsole(t)

	req := httptest.NewRequest(http.MethodGet, "/console/login", nil)
	req.AddCookie(&http.Cookie{Name: loginCSRFCookieName, Value: "planted-value"})
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)

	if got := loginCSRFTokenFrom(t, rr.Body.String()); got == "planted-value" {
		t.Fatal("a malformed cookie value was adopted as the CSRF token")
	}
}

// TestLoginCSRF_CookieIsScopedAndProtected pins the cookie attributes that make
// the construction sound: unreadable to script, and not sent with authenticated
// console traffic.
func TestLoginCSRF_CookieIsScopedAndProtected(t *testing.T) {
	tc := newTestConsole(t)
	_, jar := loginPage(t, tc)

	for _, ck := range jar {
		if ck.Name != loginCSRFCookieName {
			continue
		}
		if !ck.HttpOnly {
			t.Error("pre-session cookie is not HttpOnly: script could read the token")
		}
		if ck.Path != "/console/login" {
			t.Errorf("pre-session cookie Path = %q, want /console/login", ck.Path)
		}
		if ck.SameSite != http.SameSiteLaxMode {
			t.Errorf("pre-session cookie SameSite = %v, want Lax", ck.SameSite)
		}
		return
	}
	t.Fatalf("login page set no %q cookie", loginCSRFCookieName)
}
