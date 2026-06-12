// testhelpers_test.go wires the console against a real temp-SQLite store
// (repo convention: no mocks — identity, authoring, and analytics are the
// production services over store.Open in t.TempDir()). The engine and
// event bus stay nil: the chassis only exercises store-backed paths
// (authoring.Create/List, analytics queries, identity auth), never
// Publish or the projector.
package console

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// testConsole bundles everything a console test needs: the constructed
// Console (white-box access), its mounted handler, the bootstrap API key,
// and the minted Scope + store for seeding.
type testConsole struct {
	c      *Console
	h      http.Handler
	key    string
	scope  domain.Scope
	st     *store.Store
	deps   Deps
	dbPath string
}

// newTestConsole boots a fresh tenant on a temp SQLite store and mounts
// the console over it.
func newTestConsole(t *testing.T) *testConsole {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "notify.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	deps := Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Identity:  id,
		Authoring: authoring.New(st, nil),
		Analytics: analytics.New(st, nil),
		MasterKey: masterKey,
	}
	c := New(deps)
	return &testConsole{
		c:      c,
		h:      c.Routes(),
		key:    key,
		scope:  scope,
		st:     st,
		deps:   deps,
		dbPath: dbPath,
	}
}

// revokeAllKeys deletes every API key row by opening the temp SQLite file
// directly (the store does not — and should not — export raw handles).
func (tc *testConsole) revokeAllKeys(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", tc.dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("DELETE FROM api_keys"); err != nil {
		t.Fatalf("revoke keys: %v", err)
	}
}

// login performs the real POST /console/login flow and returns the
// session cookie plus the per-session CSRF token (opened white-box from
// the cookie — the token never appears outside the encrypted payload and
// rendered pages).
func (tc *testConsole) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	form := url.Values{"api_key": {tc.key}}
	req := httptest.NewRequest(http.MethodPost, "/console/login",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	var cookie *http.Cookie
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == sessionCookieName {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatalf("login: no %s cookie set", sessionCookieName)
	}
	s, ok := tc.c.openSession(cookie.Value)
	if !ok {
		t.Fatalf("login: session cookie did not open")
	}
	return cookie, s.CSRF
}

// get issues an authenticated GET and returns the recorder.
func (tc *testConsole) get(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	return rr
}

// seedDeliveries inserts analytics rows for the test scope.
func (tc *testConsole) seedDeliveries(t *testing.T, rows []domain.AnalyticsDelivery) {
	t.Helper()
	ctx := context.Background()
	for _, row := range rows {
		if err := tc.st.Analytics().Insert(ctx, tc.scope, row); err != nil {
			t.Fatalf("seed analytics: %v", err)
		}
	}
}

// seedWorkflow creates one workflow row through the authoring service
// (Create never touches the engine; Publish — which would — is not
// called).
func (tc *testConsole) seedWorkflow(t *testing.T, name, trigger string) {
	t.Helper()
	_, err := tc.deps.Authoring.Create(context.Background(), tc.scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
		Steps: []domain.Step{{
			StepID:        "s1",
			Type:          "in_app",
			ControlValues: json.RawMessage(`{"body":"hi"}`),
		}},
	})
	if err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
}
