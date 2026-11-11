package notify_test

// Methodology: mount the full notify handler (real routing, real prefix strip)
// under a BasePath, drive the console like a browser (redirect, login page,
// login POST, authenticated pages, a Datastar fragment endpoint) and scan every
// response for an emitted path -- Location, Set-Cookie Path, and any rooted
// path in an href/src/action/Datastar/JS string in the body. With BasePath
// "/notify" none may name a path outside the prefix; with "" the same flow must
// still emit plain /console/... paths (the no-change guard).

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/notify"
)

// consoleHarness is a notify server over a pre-bootstrapped tenant.
type consoleHarness struct {
	h      http.Handler
	base   string
	apiKey string
	slug   string
	paths  []emitted
}

// emitted is one path the console told the browser about.
type emitted struct {
	via  string // where it was found, for failure messages
	path string
}

// newConsoleHarness bootstraps a tenant and one workflow directly in the
// SQLite file, then opens notify over it (migrations are idempotent), so the
// test holds a real API key without needing an HTTP key-minting route.
func newConsoleHarness(t *testing.T, base string) *consoleHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "notify.db")
	masterKey := make([]byte, 32)

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	key, scope, err := identity.New(st, masterKey).Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const slug = "welcome-flow"
	if _, err := authoring.New(st, nil).Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome flow",
		TriggerIdentifier: slug,
		Steps:             []domain.Step{{StepID: "s1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	nc, err := nats.Connect(dagtest.StartSidecar(t), engine.InboxOption())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	n, err := notify.New(ctx, notify.Host{NC: nc, BasePath: base},
		notify.Config{DBPath: dbPath, MasterKey: masterKey})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	return &consoleHarness{h: n.Handler(), base: base, apiKey: key, slug: slug}
}

// rootedPath finds a quoted or parenthesised rooted path such as
// "/console/x", '/console/x' or (/assets/x) in decoded markup or JS.
var rootedPath = regexp.MustCompile(`["'(](/[A-Za-z][^"'<>\s)]*)`)

// do serves one request and records every path the response emits.
func (c *consoleHarness) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	label := req.Method + " " + req.URL.Path
	if loc := rec.Header().Get("Location"); loc != "" {
		c.paths = append(c.paths, emitted{label + " Location", loc})
	}
	for _, ck := range rec.Result().Cookies() {
		c.paths = append(c.paths, emitted{label + " Set-Cookie " + ck.Name + " Path", ck.Path})
	}
	body := strings.ReplaceAll(html.UnescapeString(rec.Body.String()), `\/`, `/`)
	for _, m := range rootedPath.FindAllStringSubmatch(body, -1) {
		c.paths = append(c.paths, emitted{label + " body", m[1]})
	}
	return rec
}

func (c *consoleHarness) get(t *testing.T, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, c.base+path, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	return c.do(t, req)
}

// login performs the browser login dance and returns the session cookie.
func (c *consoleHarness) login(t *testing.T) *http.Cookie {
	t.Helper()
	page := c.get(t, "/console/login")
	if page.Code != http.StatusOK {
		t.Fatalf("GET login = %d, want 200", page.Code)
	}
	m := regexp.MustCompile(`name="csrf_token"[^>]*value="([^"]+)"|value="([^"]+)"[^>]*name="csrf_token"`).
		FindStringSubmatch(page.Body.String())
	if m == nil {
		t.Fatalf("login page has no csrf_token field")
	}
	token := m[1] + m[2]
	form := url.Values{"api_key": {c.apiKey}, "csrf_token": {token}}
	req := httptest.NewRequest(http.MethodPost, c.base+"/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, ck := range page.Result().Cookies() {
		req.AddCookie(ck)
	}
	rec := c.do(t, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST login = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "notify_console_session" {
			return ck
		}
	}
	t.Fatalf("login set no session cookie: %v", rec.Result().Cookies())
	return nil
}

// browse drives the console flow every test below asserts over.
func (c *consoleHarness) browse(t *testing.T) {
	t.Helper()
	if rec := c.get(t, "/console/"); rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated console = %d, want 303 to login", rec.Code)
	}
	session := c.login(t)
	for _, p := range []string{
		"/console/",
		"/console/workflows",
		"/console/workflows/" + c.slug,
		"/console/activity",
		"/console/agents",
		"/console/agents/new",
		"/console/subscribers",
		"/console/topics",
		"/console/integrations",
		"/console/webhooks",
		"/console/stream-sources",
		"/console/a2a-keys",
		"/console/analytics",
		"/console/variables",
		"/console/contexts",
		"/console/layouts",
		"/console/translations",
		"/console/domains",
		"/console/conversations",
		"/console/settings/environments",
		"/console/settings/api-keys",
		"/console/workflows/new",
		"/console/workflows/" + c.slug + "/test",
		"/console/no-such-page",                 // not-found page renders the full layout
		"/console/api/fragments/workflows-list", // Datastar SSE patch
		"/console/api/fragments/activity-list",
	} {
		if rec := c.get(t, p, session); rec.Code >= http.StatusInternalServerError {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
	}
	c.mutate(t, session)
}

// mutate drives the redirect-after-POST paths: a rejected create (error
// redirect) and logout (redirect plus cookie expiry).
func (c *consoleHarness) mutate(t *testing.T, session *http.Cookie) {
	t.Helper()
	dash := c.get(t, "/console/", session).Body.String()
	m := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(dash)
	if m == nil {
		t.Fatalf("dashboard has no csrf_token field")
	}
	for _, p := range []string{"/console/topics", "/console/logout"} {
		form := url.Values{"csrf_token": {m[1]}, "key": {"Bad Key!"}, "name": {"x"}}
		req := httptest.NewRequest(http.MethodPost, c.base+p, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		if rec := c.do(t, req); rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d, want 303: %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestConsoleBasePath_NoPathEscapesPrefix(t *testing.T) {
	c := newConsoleHarness(t, "/notify")
	c.browse(t)

	if len(c.paths) < 100 {
		t.Fatalf("scanned only %d emitted paths; the scan is not seeing the console", len(c.paths))
	}
	var outside int
	for _, e := range c.paths {
		if e.path != "/notify" && !strings.HasPrefix(e.path, "/notify/") {
			outside++
			t.Errorf("%s emits %q outside /notify", e.via, e.path)
		}
	}
	if outside > 0 {
		t.Fatalf("%d of %d emitted paths escape the prefix", outside, len(c.paths))
	}
}

func TestConsoleBasePath_LoginSurfaces(t *testing.T) {
	c := newConsoleHarness(t, "/notify")

	rec := c.get(t, "/console/")
	if loc := rec.Header().Get("Location"); loc != "/notify/console/login" {
		t.Errorf("login redirect Location = %q, want /notify/console/login", loc)
	}
	page := c.get(t, "/console/login").Body.String()
	if !strings.Contains(page, `action="/notify/console/login"`) {
		t.Errorf("login form action not prefixed:\n%s", page)
	}
	if !regexp.MustCompile(`href="/notify/console/assets/app\.[0-9a-f]+\.css"`).MatchString(page) {
		t.Errorf("login stylesheet not prefixed")
	}

	session := c.login(t)
	if session.Path != "/notify/console" {
		t.Errorf("session cookie Path = %q, want /notify/console", session.Path)
	}
	for _, e := range c.paths {
		if strings.Contains(e.via, "Set-Cookie") && !strings.HasPrefix(e.path, "/notify/console") {
			t.Errorf("%s = %q, want under /notify/console", e.via, e.path)
		}
	}
}

func TestConsoleBasePath_EmptyPrefixUnchanged(t *testing.T) {
	c := newConsoleHarness(t, "")
	c.browse(t)

	var plain int
	for _, e := range c.paths {
		if strings.HasPrefix(e.path, "/notify") {
			t.Errorf("%s emits %q with an empty BasePath", e.via, e.path)
		}
		if e.path == "/console" || strings.HasPrefix(e.path, "/console/") {
			plain++
		}
	}
	if plain < 100 {
		t.Fatalf("only %d plain /console paths emitted at BasePath \"\"", plain)
	}
	if loc := c.get(t, "/console/").Header().Get("Location"); loc != "/console/login" {
		t.Errorf("login redirect Location = %q, want /console/login", loc)
	}
}
