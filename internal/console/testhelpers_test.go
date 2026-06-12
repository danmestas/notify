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
	"github.com/danmestas/notify/internal/connectivity"
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

	conn := connectivity.New(st, masterKey)
	deps := Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Identity:      id,
		Authoring:     authoring.New(st, nil),
		Analytics:     analytics.New(st, nil),
		Notifications: st.Notifications(),
		Messages:      st.Messages(),
		Integrations:  st.Integrations(),
		Connectivity:  conn,
		MasterKey:     masterKey,
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

// testBackground returns a context.Background for use in test helper calls
// that don't have a *testing.T in scope. This keeps test code readable.
func testBackground() context.Context { return context.Background() }

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
	tc.seedWorkflowInScope(t, tc.scope, name, trigger)
}

// seedWorkflowInScope creates a workflow under an explicit scope.
func (tc *testConsole) seedWorkflowInScope(t *testing.T, scope domain.Scope, name, trigger string) {
	t.Helper()
	_, err := tc.deps.Authoring.Create(context.Background(), scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
		Steps: []domain.Step{{
			StepID:        "s1",
			Type:          "in_app",
			ControlValues: json.RawMessage(`{"body":"hi"}`),
		}},
	})
	if err != nil {
		t.Fatalf("seed workflow in scope %+v: %v", scope, err)
	}
}

// altScope bootstraps a second tenant (distinct org + env) on the same
// store and returns its Scope. Used by scope-isolation tests: a workflow
// seeded under altScope must never surface for the logged-in tc.scope.
func (tc *testConsole) altScope(t *testing.T) domain.Scope {
	t.Helper()
	_, scope, err := tc.deps.Identity.Bootstrap(context.Background(), "OtherCorp", "prod")
	if err != nil {
		t.Fatalf("alt scope bootstrap: %v", err)
	}
	if scope.OrgID == tc.scope.OrgID && scope.EnvID == tc.scope.EnvID {
		t.Fatalf("alt scope collided with the test scope: %+v", scope)
	}
	return scope
}

// seedNotification creates a notification row directly via the store for
// activity-feed tests. Returns the persisted notification.
func (tc *testConsole) seedNotification(t *testing.T, n domain.Notification) domain.Notification {
	t.Helper()
	created, err := tc.st.Notifications().Create(context.Background(), tc.scope, n)
	if err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return created
}

// seedMessage creates a message row directly via the store for activity
// detail-panel tests.
func (tc *testConsole) seedMessage(t *testing.T, m domain.Message) domain.Message {
	t.Helper()
	created, err := tc.st.Messages().Create(context.Background(), tc.scope, m)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	return created
}

// seedIntegration creates an integration directly via the store for
// integrations-page tests. EncCredentials must be supplied by the caller
// (use []byte("fake-ciphertext") for unit tests that never decrypt).
func (tc *testConsole) seedIntegration(t *testing.T, in domain.Integration) domain.Integration {
	t.Helper()
	created, err := tc.st.Integrations().Create(context.Background(), tc.scope, in)
	if err != nil {
		t.Fatalf("seed integration: %v", err)
	}
	return created
}

// postForm issues an authenticated form POST and returns the recorder.
func (tc *testConsole) postForm(t *testing.T, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	tc.h.ServeHTTP(rr, req)
	return rr
}
