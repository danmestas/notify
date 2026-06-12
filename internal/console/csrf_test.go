// csrf_test.go pins the mutation guard: every non-GET console request
// must echo the per-session CSRF token via header or form field; a
// missing or wrong token gets the documented 403 JSON body.
//
// Methodology: real login flow (no mocks) so the token under test is the
// one the session cookie actually carries; /console/logout is the
// chassis's only mutation and serves as the guarded endpoint.
package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postLogout issues POST /console/logout with the given cookie and
// token transports.
func postLogout(
	t *testing.T, tc *testConsole,
	cookie *http.Cookie, headerToken, formToken string,
) *httptest.ResponseRecorder {
	t.Helper()
	var body strings.Reader
	if formToken != "" {
		body = *strings.NewReader("csrf_token=" + formToken)
	}
	req := httptest.NewRequest(http.MethodPost, "/console/logout", &body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if headerToken != "" {
		req.Header.Set("X-CSRF-Token", headerToken)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	return rr
}

func TestCSRF_MissingTokenForbidden(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := postLogout(t, tc, cookie, "", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no token: status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "csrf_invalid") {
		t.Errorf("no token: body missing csrf_invalid: %s", rr.Body.String())
	}
}

func TestCSRF_WrongTokenForbidden(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := postLogout(t, tc, cookie, "", "0000000000000000")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("wrong token: status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "csrf_invalid") {
		t.Errorf("wrong token: body missing csrf_invalid: %s", rr.Body.String())
	}
}

func TestCSRF_FormTokenPasses(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := postLogout(t, tc, cookie, "", csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("form token: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("form token: Location = %q, want /console/login", got)
	}
}

func TestCSRF_HeaderTokenPasses(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := postLogout(t, tc, cookie, csrf, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("header token: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("header token: Location = %q, want /console/login", got)
	}
}

func TestCSRF_GETExempt(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	// No token on a GET must still serve the page — the policy is
	// mutation-only.
	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Errorf("GET without token: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Dashboard") {
		t.Errorf("GET without token: dashboard body missing")
	}
}

func TestCSRF_RenderedLogoutFormCarriesToken(t *testing.T) {
	tc := newTestConsole(t)
	cookie, csrf := tc.login(t)
	rr := tc.get(t, "/console/", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `name="csrf_token" value="`+csrf+`"`) {
		t.Errorf("dashboard layout missing the session CSRF token in the logout form")
	}
}
