package console

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/webhooksout"
)

// devMode reflects NOTIFY_DEV=1 at process start. When true the asset
// handlers send no-cache headers so CSS/JS edits show up on reload without
// manually busting the browser cache. Read once — the value cannot change
// at runtime (dagnats handler.go idiom).
var devMode = os.Getenv("NOTIFY_DEV") == "1"

// assetCacheHeader returns the Cache-Control value every asset handler
// sets: long+immutable in production, no-store in dev mode.
func assetCacheHeader() string {
	if devMode {
		return "no-store, must-revalidate"
	}
	return "public, max-age=31536000, immutable"
}

// Deps carries everything New needs to wire the console. All services are
// called in-process — the console never hops through its own HTTP API.
type Deps struct {
	// Logger is required; the console logs via slog only.
	Logger *slog.Logger
	// Identity authenticates pasted API keys and resolves their Scope.
	Identity *identity.Service
	// Authoring lists workflows for the dashboard KPI.
	Authoring *authoring.Service
	// Analytics answers the deliveries-by-status and 7-day time-series
	// queries the dashboard renders. The caller picks the backend
	// (SQLite or DuckDB) exactly as it does for httpapi.
	Analytics *analytics.Service
	// MasterKey is the 32-byte AES-256-GCM key the session cookie is
	// sealed with — the same master key identity and connectivity use.
	MasterKey []byte
	// DagnatsConsoleURL is the deep-link base for the engine console
	// (NOTIFY_DAGNATS_CONSOLE_URL). Empty means "no engine console
	// reachable": the runURL template helper renders nothing, so pages
	// degrade to plain text instead of dead links. Plumbed here for the
	// activity feed slice (W1-6).
	DagnatsConsoleURL string
	// Notifications provides the NotificationRepo for the activity feed
	// (W1-6). Nil is allowed only when no activity routes are mounted;
	// in practice New panics when Notifications is nil (activity page
	// requires it).
	Notifications store.NotificationRepo
	// Messages provides the MessageRepo for the activity detail panel
	// (W1-6); used for the transactionId-workaround message lookup.
	Messages store.MessageRepo
	// Integrations provides the IntegrationRepo for the integrations store
	// page (W2). List / GetByID / SetActive / Delete are called directly;
	// credential-merge updates go through Connectivity.Update.
	Integrations store.IntegrationRepo
	// Connectivity is the connectivity service used by the integrations page
	// for Update (credential merge + primary-conflict detection). Nil is
	// allowed only when no integration mutation routes are mounted; in
	// practice New panics when Integrations is non-nil and Connectivity is nil.
	Connectivity *connectivity.Service
	// Audience is the audience service for subscriber and topic management
	// (W2-C3, W2-C4, W2-C5). Nil is allowed only when no subscriber/topic
	// routes are mounted; in practice New panics when Audience is nil.
	Audience *audience.Service
	// Webhooks is the webhook management service for CRUD (W2-D2).
	// Nil is allowed only when no webhook routes are mounted.
	Webhooks *webhooksout.ManagementService
	// ReadOnly disables every mutation button via the actionButton
	// template helper and renders the read-only banner. The chassis has
	// no mutations beyond login/logout (which stay enabled — an operator
	// must always be able to sign out); the flag exists so later slices
	// inherit one switch instead of inventing their own.
	ReadOnly bool
}

// Console is the operator-console handler factory. Construct with New;
// mount Routes() at /console on the public listener.
type Console struct {
	deps Deps
	gcm  cipher.AEAD
	ts   *templateSet
}

// New builds a Console from deps. It panics on programmer errors (nil
// services, bad master key, unparseable embedded templates) so
// misconfiguration fails loudly at startup, not at first request.
func New(deps Deps) *Console {
	if deps.Logger == nil {
		panic("console.New: deps.Logger is nil")
	}
	if deps.Identity == nil {
		panic("console.New: deps.Identity is nil")
	}
	if deps.Authoring == nil {
		panic("console.New: deps.Authoring is nil")
	}
	if deps.Analytics == nil {
		panic("console.New: deps.Analytics is nil")
	}
	if deps.Notifications == nil {
		panic("console.New: deps.Notifications is nil")
	}
	if deps.Messages == nil {
		panic("console.New: deps.Messages is nil")
	}
	if deps.Integrations != nil && deps.Connectivity == nil {
		panic("console.New: Connectivity must be set when Integrations is provided")
	}
	if deps.Audience == nil {
		panic("console.New: deps.Audience is nil")
	}
	if len(deps.MasterKey) != 32 {
		panic(fmt.Sprintf("console.New: MasterKey must be 32 bytes for AES-256-GCM, got %d", len(deps.MasterKey)))
	}
	block, err := aes.NewCipher(deps.MasterKey)
	if err != nil {
		panic(fmt.Sprintf("console.New: aes.NewCipher: %v", err))
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("console.New: cipher.NewGCM: %v", err))
	}
	ts, err := loadTemplates()
	if err != nil {
		panic(fmt.Sprintf("console.New: load templates: %v", err))
	}
	c := &Console{deps: deps, gcm: gcm, ts: ts}
	// Bind the Console-carrying helpers (runURL) once, before any request
	// executes a template — Funcs during render would race concurrent
	// executions.
	c.ts.base.Funcs(c.funcMap())
	for _, tmpl := range c.ts.pageTemplates {
		tmpl.Funcs(c.funcMap())
	}
	return c
}

// Routes returns the console's http.Handler: a chi sub-router carrying
// every /console/* path. Callers mount it on the top-level mux:
//
//	mux.Handle("/console", c.Routes())
//	mux.Handle("/console/", c.Routes())
//
// Route inventory (the conventions every later slice extends):
//
//	GET  /console/login                                   login page  (no session)
//	POST /console/login                                   authenticate (no session, no CSRF — it presents the credential)
//	GET  /console/assets/*                                embedded assets (no session)
//	GET  /console/ , /console                             dashboard
//	POST /console/logout                                  clear session (CSRF)
//	GET  /console/api/fragments/dashboard-kpis            KPI fragment (PatchElements SSE)
//	GET  /console/workflows                               workflows list page
//	GET  /console/api/fragments/workflows-list            workflows list fragment (PatchElements SSE)
//	GET  /console/workflows/{slug}                        workflow detail page (read-only)
//	GET  /console/analytics                               analytics page
//	GET  /console/api/fragments/analytics-kpis            analytics KPI fragment (PatchElements SSE)
//	GET  /console/api/fragments/analytics-charts          analytics charts fragment (PatchElements SSE)
//	*                                                     layout-wrapped 404
//	GET  /console/login                                 login page (no session)
//	POST /console/login                                 authenticate (no session, no CSRF)
//	GET  /console/assets/*                              embedded assets (no session)
//	GET  /console/ , /console                           dashboard
//	POST /console/logout                                clear session (CSRF)
//	GET  /console/api/fragments/dashboard-kpis          KPI fragment (PatchElements SSE)
//	GET  /console/activity                              activity feed page (W1-6)
//	GET  /console/api/fragments/activity-list           paginated feed fragment (PatchElements SSE)
//	GET  /console/api/fragments/activity-detail         per-notification detail panel (PatchElements SSE)
//	*                                                   layout-wrapped 404
func (c *Console) Routes() http.Handler {
	if c == nil {
		panic("Console.Routes: c is nil")
	}
	r := chi.NewRouter()

	r.Get("/console/login", c.serveLoginPage)
	r.Post("/console/login", c.serveLoginSubmit)
	r.Get("/console/assets/datastar.js", serveGzAsset("datastar.js.gz",
		"application/javascript; charset=utf-8"))
	r.Get("/console/assets/app.css", servePlainAsset("app.css",
		"text/css; charset=utf-8"))
	r.Get("/console/assets/theme.js", servePlainAsset("theme.js",
		"application/javascript; charset=utf-8"))

	r.Group(func(pr chi.Router) {
		pr.Use(c.sessionMiddleware)
		pr.Use(c.csrfMiddleware)
		pr.Get("/console", c.serveDashboard)
		pr.Get("/console/", c.serveDashboard)
		pr.Post("/console/logout", c.serveLogout)
		pr.Get("/console/api/fragments/dashboard-kpis", c.serveFragmentDashboardKPIs)
		pr.Get("/console/workflows", c.serveWorkflowsList)
		pr.Get("/console/api/fragments/workflows-list", c.serveFragmentWorkflowsList)
		pr.Get("/console/workflows/{slug}", c.serveWorkflowDetail)
		pr.Get("/console/analytics", c.servePageAnalytics)
		pr.Get("/console/api/fragments/analytics-kpis", c.serveFragmentAnalyticsKPIs)
		pr.Get("/console/api/fragments/analytics-charts", c.serveFragmentAnalyticsCharts)
		// W1-6: activity feed
		pr.Get("/console/activity", c.serveActivityPage)
		pr.Get("/console/api/fragments/activity-list", c.serveFragmentActivityList)
		pr.Get("/console/api/fragments/activity-detail", c.serveFragmentActivityDetail)
		// W2: integrations store
		pr.Get("/console/integrations", c.serveIntegrationsPage)
		pr.Get("/console/integrations/{id}", c.serveIntegrationsPage)
		pr.Post("/console/integrations", c.handleIntegrationCreate)
		pr.Post("/console/integrations/{id}", c.handleIntegrationUpdate)
		pr.Post("/console/integrations/{id}/delete", c.handleIntegrationDelete)
		pr.Post("/console/integrations/{id}/active", c.handleIntegrationToggleActive)
		pr.Get("/console/api/fragments/integrations-grid", c.serveFragmentIntegrationsGrid)
		pr.Get("/console/api/fragments/integration-detail", c.serveFragmentIntegrationDetail)
		pr.Get("/console/api/fragments/integration-provider-form", c.serveFragmentIntegrationProviderForm)
		// W2-C3: subscriber management
		pr.Get("/console/subscribers", c.serveSubscribersPage)
		pr.Get("/console/subscribers/new", c.serveSubscriberNew)
		pr.Get("/console/subscribers/{subscriberId}", c.serveSubscriberDetail)
		pr.Post("/console/subscribers", c.handleSubscriberCreate)
		pr.Post("/console/subscribers/{subscriberId}", c.handleSubscriberUpdate)
		pr.Post("/console/subscribers/{subscriberId}/delete", c.handleSubscriberDelete)
		pr.Get("/console/api/fragments/subscribers-list", c.serveFragmentSubscribersList)
		// W2-C4: topic management
		pr.Get("/console/topics", c.serveTopicsPage)
		pr.Get("/console/topics/new", c.serveTopicNew)
		pr.Get("/console/topics/{key}", c.serveTopicDetail)
		pr.Post("/console/topics", c.handleTopicCreate)
		pr.Post("/console/topics/{key}", c.handleTopicUpdate)
		pr.Post("/console/topics/{key}/delete", c.handleTopicDelete)
		pr.Post("/console/topics/{key}/subscribers", c.handleTopicAddSubscriber)
		pr.Post("/console/topics/{key}/subscribers/{subscriberId}/remove", c.handleTopicRemoveSubscriber)
		pr.Get("/console/api/fragments/topics-list", c.serveFragmentTopicsList)
		pr.Get("/console/api/fragments/subscriber-autocomplete", c.serveFragmentSubscriberAutocomplete)
		// W2-D2: webhook endpoint management
		pr.Get("/console/webhooks", c.serveWebhooksPage)
		pr.Get("/console/webhooks/new", c.serveWebhookNew)
		pr.Post("/console/webhooks", c.handleWebhookCreate)
		pr.Post("/console/api/webhooks/{id}/toggle", c.handleWebhookToggle)
		pr.Post("/console/api/webhooks/{id}/delete", c.handleWebhookDelete)
		pr.Post("/console/api/webhooks/{id}", c.handleWebhookUpdate)
		pr.Get("/console/api/fragments/webhooks-list", c.serveFragmentWebhooksList)
	})

	// Unmatched /console/* paths go through the full session gate, same
	// as any registered route: anonymous (and revoked-key) requests are
	// redirected to login so the 404 page never leaks whether a path
	// exists, and authenticated operators get the layout-wrapped 404
	// with a live session on the context — which the layout's logout
	// form needs for its CSRF token.
	r.NotFound(c.sessionMiddleware(http.HandlerFunc(c.serveNotFound)).ServeHTTP)
	return r
}

// serveGzAsset streams a gzip-on-disk embedded asset with
// Content-Encoding: gzip; the browser decompresses transparently
// (dagnats / Scalar pattern).
func serveGzAsset(name, contentType string) http.HandlerFunc {
	if name == "" {
		panic("serveGzAsset: name is empty")
	}
	if contentType == "" {
		panic("serveGzAsset: contentType is empty")
	}
	body, err := fs.ReadFile(assetsFS, "assets/"+name)
	if err != nil {
		panic(fmt.Sprintf("serveGzAsset: read %s: %v", name, err))
	}
	if len(body) == 0 {
		panic(fmt.Sprintf("serveGzAsset: %s is empty", name))
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Cache-Control", assetCacheHeader())
		_, _ = w.Write(body)
	}
}

// servePlainAsset is the uncompressed variant for app.css, which is small
// enough that gzip would only complicate the embed path.
func servePlainAsset(name, contentType string) http.HandlerFunc {
	if name == "" {
		panic("servePlainAsset: name is empty")
	}
	if contentType == "" {
		panic("servePlainAsset: contentType is empty")
	}
	body, err := fs.ReadFile(assetsFS, "assets/"+name)
	if err != nil {
		panic(fmt.Sprintf("servePlainAsset: read %s: %v", name, err))
	}
	if len(body) == 0 {
		panic(fmt.Sprintf("servePlainAsset: %s is empty", name))
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", assetCacheHeader())
		_, _ = w.Write(body)
	}
}
