// handler_test.go is the route-table auth smoke: it walks the chi route
// inventory and asserts every registered route sits behind the session
// gate except the login pair and the static assets. New routes added by
// later slices inherit the assertion automatically — forgetting the
// middleware group fails this test, not a pen test.
package console

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// paramRE matches chi path parameters like {slug} or {id} so the walker
// can substitute a routable placeholder without changing the auth check.
var paramRE = regexp.MustCompile(`\{[^}]+\}`)

// authExempt enumerates the only routes allowed to answer without a
// session. Everything else must 303 to /console/login.
var authExempt = map[string]bool{
	"GET /console/login":              true,
	"POST /console/login":             true,
	"GET /console/assets/datastar.js": true,
	"GET /console/assets/app.css":     true,
	"GET /console/assets/theme.js":    true,
}

func TestRouteTable_EverythingBehindAuthExceptLoginAndAssets(t *testing.T) {
	tc := newTestConsole(t)
	router, ok := tc.h.(chi.Router)
	if !ok {
		t.Fatalf("Routes() did not return a chi.Router (got %T)", tc.h)
	}

	visited := 0
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		visited++
		// Substitute a non-empty placeholder for every path param so the
		// route is routable. The auth check does not depend on the specific
		// value — it fires before any param is read.
		requestRoute := route
		if strings.Contains(route, "{") {
			requestRoute = paramRE.ReplaceAllString(route, "x")
		}
		req := httptest.NewRequest(method, requestRoute, nil)
		rr := httptest.NewRecorder()
		tc.h.ServeHTTP(rr, req)

		if authExempt[method+" "+route] {
			if rr.Code == http.StatusSeeOther {
				t.Errorf("%s %s: exempt route redirected to login", method, route)
			}
			return nil
		}
		if rr.Code != http.StatusSeeOther {
			t.Errorf("%s %s: status = %d, want 303 (unauthenticated must redirect)", method, route, rr.Code)
			return nil
		}
		if got := rr.Header().Get("Location"); got != "/console/login" {
			t.Errorf("%s %s: Location = %q, want /console/login", method, route, got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if visited < 16 {
		t.Errorf("walked %d routes; expected at least 10 — did the route table shrink?", visited)
	}
}

func TestRouteTable_AssetsServeWithoutSession(t *testing.T) {
	tc := newTestConsole(t)

	rr := tc.get(t, "/console/assets/app.css", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("app.css: status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), ":root") {
		t.Errorf("app.css body missing the :root palette block")
	}

	rr = tc.get(t, "/console/assets/datastar.js", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("datastar.js: status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("datastar.js: Content-Encoding = %q, want gzip", got)
	}
}
