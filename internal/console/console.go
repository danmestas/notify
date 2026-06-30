package console

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/chatloop"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/contexts"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/preferences"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/webhooksout"
)

// TriggerIngester is the subset of trigger.Service the console uses. The
// production wiring passes *trigger.Service directly; tests may supply a
// lightweight stub without a live engine.
type TriggerIngester interface {
	Ingest(ctx context.Context, scope domain.Scope, req trigger.TriggerRequest) (trigger.TriggerResponse, error)
}

// ConversationComposer is the inbox compose port the conversation view's
// send-message form relays through (Wave 3). *ingress.Service satisfies it
// structurally; the form POSTs to a console route that calls ComposeMessage
// in-process and 303-redirects back — no SSE, no loopback HTTP. Nil disables
// the compose box (the form renders a "service unavailable" notice instead).
type ConversationComposer interface {
	ComposeMessage(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (string, error)
}

// ChatStatusProvider is the read-only port the /console/chat status page reads
// (#106). *chatloop.Service satisfies it structurally; the console renders the
// returned snapshot — it never reaches into chatloop internals. Nil leaves the
// chat status route unmounted (no nav entry surfaced via the live-route check).
type ChatStatusProvider interface {
	Status(ctx context.Context, scope domain.Scope) chatloop.Snapshot
}

// CredentialVerifier is the durable credential-verify port the integration
// drawer's "Verify credentials" action calls (W4 ADR-013, #55). It kicks an
// engine run that probes the integration's provider credentials and returns the
// new dagnats run id the console deep-links to. *credverify.Service satisfies it
// in production; tests supply a stub. Nil renders the Verify button disabled.
type CredentialVerifier interface {
	Start(ctx context.Context, scope domain.Scope, integrationID string) (string, error)
}

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
	// AnthropicKey is the Anthropic API key (ANTHROPIC_API_KEY) that powers the
	// AI workflow builder's Guided create tab (B2). Empty means "AI builder not
	// configured": New leaves the aiBuilder nil, the Guided tab shows a
	// not-configured note, and the ai-generate endpoint returns a clear message
	// instead of panicking. Wired from os.Getenv at cmd/notify/main.go.
	AnthropicKey string
	// BuilderSessions persists AI workflow-builder sessions (prompt + proposed
	// draft) for the Guided create flow's generate -> preview -> confirm loop.
	// Nil disables the AI builder endpoints (treated as not configured).
	BuilderSessions store.BuilderSessionRepo
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
	// Contexts is the context service for the contexts CRUD pages (W4-C1).
	// Nil is allowed only when no contexts routes are mounted.
	Contexts *contexts.Service
	// Layouts provides layout CRUD for the layouts pages (W4-L1).
	// Nil is allowed only when no layout routes are mounted.
	Layouts store.LayoutRepo
	// Translations provides translation CRUD for the translations pages (W4-L2).
	// Nil is allowed only when no translation routes are mounted.
	Translations store.TranslationRepo
	// Environments provides env-level settings reads/writes (translation
	// settings W4-L2). Nil means the settings panel shows read-only with
	// empty defaults.
	Environments store.EnvironmentRepo
	// EnvVars is the env vars service for the variables CRUD pages (W4-V1).
	// Nil is allowed only when no variables routes are mounted.
	EnvVars *envvars.Service
	// InboundDomains is the inbound domain management service (W4-D1, #53).
	// Nil is allowed only when no domain routes are mounted.
	InboundDomains *inbounddomains.Service
	// Preferences is the preferences service used by the subscriber Preferences
	// tab (W4-S1 #40). Nil is allowed only when no preferences routes are
	// mounted; in practice the subscriber detail page renders a stub when nil.
	Preferences *preferences.Service
	// TopicTriggers provides the topic_triggers projection repo for the Topic
	// Activity tab (W4-S2 #41). Nil renders an empty-state stub.
	TopicTriggers store.TopicTriggerRepo
	// Trigger is the trigger service used by the workflow test/trigger page
	// (W2-A5). The console calls Ingest in-process — no loopback HTTP.
	// Nil disables test-trigger POSTs (returns "service unavailable").
	// The production wiring passes *trigger.Service; tests may pass a stub.
	Trigger TriggerIngester
	// CredVerifier kicks the durable credential-verify run for the integration
	// drawer's "Verify credentials" action (W4 ADR-013, #55). Nil renders the
	// Verify button disabled with a tooltip. The production wiring passes
	// *credverify.Service; tests may pass a stub.
	CredVerifier CredentialVerifier
	// Agents provides the agents store for the agents CRUD pages and the
	// conversation views (Wave 3). Nil is allowed only when no agent routes
	// are mounted; in practice New panics when Agents is nil.
	Agents *agentstore.Store
	// Composer relays the conversation view's send-message form through the
	// ingress compose path in-process (Wave 3). Nil disables the compose box.
	Composer ConversationComposer
	// ChatStatus exposes the chat two-way loop's read-only config snapshot for
	// the /console/chat status page (#106). The production wiring passes the
	// *chatloop.Service; tests pass a stub. Nil leaves the chat route unmounted.
	ChatStatus ChatStatusProvider
	// EmailThreads provides the inbound email thread log for the
	// /console/inbound-email page. Nil leaves the inbound-email route unmounted.
	EmailThreads store.EmailThreadRepo
	// InboundSecretConfigured is true when NOTIFY_INBOUND_SECRET is set, i.e. the
	// /v1/inbound/email endpoint is live. False renders the "endpoint disabled"
	// warning. The console never sees the secret value.
	InboundSecretConfigured bool
	// PublicBaseURL is the server's public origin (e.g.
	// "https://notify.example.com"). The agent channels tab uses it to render a
	// ready-to-register inbound webhook URL for each linked chat integration,
	// mirroring the /v1 agents API's channelSetup contract. Empty string falls
	// back to the "{base_url}" placeholder the operator fills in.
	PublicBaseURL string
	// ReadOnly disables every mutation button via the actionButton
	// template helper and renders the read-only banner. The chassis has
	// no mutations beyond login/logout (which stay enabled — an operator
	// must always be able to sign out); the flag exists so later slices
	// inherit one switch instead of inventing their own.
	ReadOnly bool
	// HTTPAddr is the server's bound address (e.g. ":3000" or
	// "https://notify.example.com"). Used by the API Keys page to show the
	// "API URL" card. Empty string renders a placeholder.
	HTTPAddr string
	// LiveTail is the eventbus port the activity SSE live-tail subscribes
	// through (W4-W2). The production wiring passes *eventbus.Bus; tests may
	// supply a stub. Nil disables the live-tail: the stream endpoint answers
	// 503 and the activity pages degrade to manual-refresh pagination.
	LiveTail DeliverySubscriber
	// AIBuilder dispatches durable AI workflow-builder runs (the Guided create
	// tab, B3) and reports their terminal run status. The production wiring passes
	// *aibuilder.AgentLoopBuilder; nil disables the Guided tab (the tab shows a
	// not-configured note and ai-generate returns a clear message).
	AIBuilder aiBuildDispatcher
	// BuildStream is the eventbus port the ai-generate handler subscribes through
	// for a run's live build Events (stream.<runID>.build). The production wiring
	// passes *eventbus.Bus; nil disables live streaming (ai-generate falls back to
	// an inline "not configured" message rather than holding a dead connection).
	BuildStream aiBuildStreamSubscriber
}

// Console is the operator-console handler factory. Construct with New;
// mount Routes() at /console on the public listener.
type Console struct {
	deps      Deps
	gcm       cipher.AEAD
	ts        *templateSet
	stepTypes *steptype.Registry
	// aiBuilder drives the Guided create tab (B3): it dispatches a durable
	// agent-loop run and reports its terminal status. It is non-nil only when the
	// production wiring injects deps.AIBuilder; a nil aiBuilder gracefully disables
	// the feature (the Guided tab shows a not-configured note, the endpoints return
	// a clear message). Tests inject a scripted mock.
	aiBuilder aiBuildDispatcher
	// buildStream is the eventbus port the ai-generate handler subscribes through
	// for a run's live build Events. Nil disables live streaming.
	buildStream aiBuildStreamSubscriber
	// aiGenerateTimeoutOverride, when non-zero, replaces aiGenerateTimeout as the
	// wall-clock cap on a single AI generation run (tests only).
	aiGenerateTimeoutOverride time.Duration
	// aiBuildPollOverride, when non-zero, replaces aiBuildPollInterval as the
	// run-status poll cadence the ai-generate handler uses to resolve a stream
	// that ended without a terminal event (tests only).
	aiBuildPollOverride time.Duration
}

// aiBuildDispatcher is the subset of *aibuilder.AgentLoopBuilder the console
// drives: start a durable build run for a session, and report whether a run
// reached a terminal failed state (to resolve a stream that ended without a
// terminal event). Tests inject a scripted mock.
type aiBuildDispatcher interface {
	Dispatch(ctx context.Context, scope domain.Scope, sessionID string) (runID string, err error)
	RunFailed(ctx context.Context, runID string) (failed bool, err error)
}

// aiBuildStreamSubscriber is the eventbus port the ai-generate handler subscribes
// through for a run's live build Events (stream.<runID>.build). *eventbus.Bus
// satisfies it structurally; tests supply a lightweight stub that replays a
// scripted event sequence.
type aiBuildStreamSubscriber interface {
	SubscribeBuildStream(runID string, handler func([]byte)) (unsubscribe func() error, err error)
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
	if deps.Agents == nil {
		panic("console.New: deps.Agents is nil")
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
	c := &Console{deps: deps, gcm: gcm, ts: ts, stepTypes: steptype.Default()}
	// Bind the Console-carrying helpers (runURL) once, before any request
	// executes a template — Funcs during render would race concurrent
	// executions.
	c.ts.base.Funcs(c.funcMap())
	for _, tmpl := range c.ts.pageTemplates {
		tmpl.Funcs(c.funcMap())
	}
	// Wire the durable AI workflow builder from the injected dispatcher + stream
	// port (B3). An unwired dispatcher leaves aiBuilder nil — the Guided tab and
	// endpoints degrade gracefully rather than panic. The dispatcher is constructed
	// in cmd/notify with the engine; the console only drives Dispatch/RunFailed and
	// the live stream.
	c.aiBuilder = deps.AIBuilder
	c.buildStream = deps.BuildStream
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
//	GET  /console/workflows/{slug}/test                   workflow test/trigger page (W2-A5)
//	POST /console/api/workflows/{slug}/test               fire a test trigger (W2-A5)
//	GET  /console/workflows/{slug}/activity               workflow activity tab (W2-A6)
//	GET  /console/api/fragments/workflow-activity/{slug}  activity tbody fragment (W2-A6)
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
//	GET  /console/api/stream/activity                   SSE live-tail (W4-W2; prepends new rows)
//	*                                                   layout-wrapped 404
func (c *Console) Routes() http.Handler {
	if c == nil {
		panic("Console.Routes: c is nil")
	}
	r := chi.NewRouter()

	r.Get("/console/login", c.serveLoginPage)
	r.Post("/console/login", c.serveLoginSubmit)
	// Asset routes use fingerprinted (content-hashed) URLs so that
	// "Cache-Control: public, max-age=31536000, immutable" is safe: the URL
	// changes whenever the content changes, so browsers always pick up new
	// CSS/JS on deploy.  The fingerprinted path is computed at init from the
	// sha256 of the embedded bytes (see assets_manifest.go).
	r.Get(assetManifest["datastar.js"], serveGzAsset("datastar.js.gz",
		"application/javascript; charset=utf-8"))
	r.Get(assetManifest["app.css"], servePlainAsset("app.css",
		"text/css; charset=utf-8"))
	r.Get(assetManifest["theme.js"], servePlainAsset("theme.js",
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
		// W2-A3: create + duplicate side-sheets. The literal `new` segment is
		// registered before the {slug} wildcard so chi never matches "new" as a
		// slug (chi prefers static segments, but the explicit order documents it).
		pr.Get("/console/workflows/new", c.serveWorkflowNew)
		pr.Get("/console/workflows/{slug}/duplicate", c.serveWorkflowDuplicate)
		pr.Get("/console/workflows/{slug}", c.serveWorkflowDetail)
		pr.Post("/console/api/workflows", c.handleWorkflowCreate)
		// B2: AI workflow builder (Guided create tab). ai-generate runs the
		// builder synchronously and patches a preview; ai-confirm compiles +
		// persists the drafted workflow; ai-discard drops the draft.
		pr.Post("/console/api/workflows/ai-generate", c.handleWorkflowAIGenerate)
		pr.Post("/console/api/workflows/ai-confirm/{sessionID}", c.handleWorkflowAIConfirm)
		pr.Post("/console/api/workflows/ai-discard/{sessionID}", c.handleWorkflowAIDiscard)
		pr.Post("/console/api/workflows/{slug}/duplicate", c.handleWorkflowDuplicate)
		// W2-A4: editor mutations. `steps/reorder` is registered before the
		// `steps/{stepId}/...` wildcards so the literal `reorder` segment wins.
		pr.Post("/console/api/workflows/{slug}/save", c.handleWorkflowSave)
		pr.Post("/console/api/workflows/{slug}/delete", c.handleWorkflowDelete)
		pr.Post("/console/api/workflows/{slug}/activate", c.handleWorkflowActivate)
		pr.Post("/console/api/workflows/{slug}/steps", c.handleWorkflowStepAdd)
		pr.Post("/console/api/workflows/{slug}/steps/reorder", c.handleWorkflowStepReorder)
		pr.Post("/console/api/workflows/{slug}/steps/{stepId}/delete", c.handleWorkflowStepDelete)
		pr.Post("/console/api/workflows/{slug}/steps/{stepId}/save", c.handleWorkflowStepSave)
		pr.Get("/console/api/fragments/workflow-step-editor/{slug}/{stepId}", c.serveFragmentWorkflowStepEditor)
		// W4-W3: Handlebars live-preview pane (read-only GET fragment; no mutation).
		pr.Get("/console/api/fragments/step-preview", c.serveFragmentStepPreview)
		// W2-A5: workflow test/trigger page
		pr.Get("/console/workflows/{slug}/test", c.serveWorkflowTestPage)
		pr.Post("/console/api/workflows/{slug}/test", c.handleWorkflowTest)
		// W2-A6: workflow activity tab
		pr.Get("/console/workflows/{slug}/activity", c.serveWorkflowActivityPage)
		pr.Get("/console/api/fragments/workflow-activity/{slug}", c.serveFragmentWorkflowActivity)
		pr.Get("/console/analytics", c.servePageAnalytics)
		pr.Get("/console/api/fragments/analytics-kpis", c.serveFragmentAnalyticsKPIs)
		pr.Get("/console/api/fragments/analytics-charts", c.serveFragmentAnalyticsCharts)
		// W1-6: activity feed
		pr.Get("/console/activity", c.serveActivityPage)
		pr.Get("/console/api/fragments/activity-list", c.serveFragmentActivityList)
		pr.Get("/console/api/fragments/activity-detail", c.serveFragmentActivityDetail)
		// W4 ADR-013 (#55): durable resend-notification action. Native form
		// POST → 303; two-step inline confirm in the detail panel (it sends a
		// real message). Re-fires the notification's workflow for the same
		// subscriber + payload through the engine, yielding a new run.
		pr.Post("/console/activity/{id}/resend", c.handleNotificationResend)
		// W4-W2: SSE live-tail — one endpoint, both activity consumers
		// (global feed + per-workflow tab). ?target=wf&workflow=<id> scopes
		// it to one workflow's tbody.
		pr.Get("/console/api/stream/activity", c.serveActivityStream)
		// W2: integrations store
		pr.Get("/console/integrations", c.serveIntegrationsPage)
		pr.Get("/console/integrations/{id}", c.serveIntegrationsPage)
		pr.Post("/console/integrations", c.handleIntegrationCreate)
		pr.Post("/console/integrations/{id}", c.handleIntegrationUpdate)
		pr.Post("/console/integrations/{id}/delete", c.handleIntegrationDelete)
		pr.Post("/console/integrations/{id}/active", c.handleIntegrationToggleActive)
		// W4 ADR-013 (#55): durable credential-verify action. Native form
		// POST → 303; two-step inline confirm in the drawer (it performs a real
		// provider handshake). Kicks an engine run that probes the credentials
		// and surfaces the run deep-link + result.
		pr.Post("/console/integrations/{id}/verify", c.handleIntegrationVerify)
		// Per-integration test-send (#…): sends ONE real message through THIS
		// integration to an operator-supplied destination; 303 back to the drawer
		// with ?testSendOK=1 or ?testSendError=<msg>.
		pr.Post("/console/integrations/{id}/test-send", c.handleIntegrationTestSend)
		pr.Get("/console/api/fragments/integrations-grid", c.serveFragmentIntegrationsGrid)
		pr.Get("/console/api/fragments/integration-detail", c.serveFragmentIntegrationDetail)
		pr.Get("/console/api/fragments/integration-activity", c.serveFragmentIntegrationActivity)
		pr.Get("/console/api/fragments/integration-provider-form", c.serveFragmentIntegrationProviderForm)
		// W2-C3: subscriber management
		pr.Get("/console/subscribers", c.serveSubscribersPage)
		pr.Get("/console/subscribers/new", c.serveSubscriberNew)
		pr.Get("/console/subscribers/{subscriberId}", c.serveSubscriberDetail)
		pr.Post("/console/subscribers", c.handleSubscriberCreate)
		pr.Post("/console/subscribers/{subscriberId}", c.handleSubscriberUpdate)
		pr.Post("/console/subscribers/{subscriberId}/delete", c.handleSubscriberDelete)
		pr.Get("/console/api/fragments/subscribers-list", c.serveFragmentSubscribersList)
		// W4-S1: subscriber preferences tab (BP-10)
		pr.Post("/console/subscribers/{subscriberId}/preferences", c.handleSubscriberPreferencesPatch)
		// subscriber channels tab: per-subscriber delivery addresses (chat/push)
		pr.Post("/console/subscribers/{subscriberId}/channels", c.handleSubscriberChannels)
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
		// W4-C2: contexts management
		if c.deps.Contexts != nil {
			pr.Get("/console/contexts", c.serveContextsPage)
			pr.Get("/console/contexts/new", c.serveContextNew)
			pr.Get("/console/contexts/{type}/{id}", c.serveContextDetail)
			pr.Post("/console/contexts", c.handleContextCreate)
			pr.Post("/console/api/contexts/{type}/{id}", c.handleContextUpdate)
			pr.Post("/console/api/contexts/{type}/{id}/delete", c.handleContextDelete)
			pr.Get("/console/api/fragments/contexts-list", c.serveFragmentContextsList)
		}
		// W4-L1: layouts list, create, editor. The literal `new` segment is
		// registered before the {key} wildcard so chi never matches "new" as
		// a layout key.
		pr.Get("/console/layouts", c.serveLayoutsPage)
		pr.Get("/console/layouts/new", c.serveLayoutsNew)
		pr.Get("/console/layouts/{key}", c.serveLayoutEditor)
		pr.Post("/console/api/layouts", c.handleLayoutCreate)
		pr.Post("/console/api/layouts/{key}/save", c.handleLayoutSave)
		pr.Post("/console/api/layouts/{key}/delete", c.handleLayoutDelete)
		pr.Post("/console/api/layouts/{key}/default", c.handleLayoutSetDefault)
		pr.Get("/console/api/fragments/layouts-list", c.serveFragmentLayoutsList)
		pr.Get("/console/api/fragments/layout-editor", c.serveFragmentLayoutEditor)
		// W4-L2: translations list, locale editor, settings. The literal
		// `new` and `settings` segments are registered before the {locale}
		// wildcard so chi never matches them as a locale code.
		if c.deps.Translations != nil {
			pr.Get("/console/translations", c.serveTranslationsPage)
			pr.Get("/console/translations/new", c.serveTranslationsNew)
			pr.Get("/console/translations/settings", c.serveTranslationsSettings)
			pr.Get("/console/translations/{locale}", c.serveLocaleEditor)
			pr.Post("/console/api/translations", c.handleTranslationLocaleCreate)
			pr.Post("/console/api/translations/{locale}/save", c.handleTranslationLocaleSave)
			pr.Post("/console/api/translations/{locale}/delete", c.handleTranslationLocaleDelete)
			pr.Post("/console/api/translations/settings", c.handleTranslationSettingsSave)
			pr.Get("/console/api/fragments/translations-list", c.serveFragmentTranslationsList)
		}
		// W4-V2: variables CRUD
		if c.deps.EnvVars != nil {
			pr.Get("/console/variables", c.serveVariablesPage)
			pr.Get("/console/variables/new", c.serveVariableNew)
			pr.Get("/console/variables/{key}", c.serveVariableDetail)
			pr.Get("/console/api/fragments/variables-list", c.serveFragmentVariablesList)
			pr.Post("/console/api/variables", c.handleVariableCreate)
			pr.Post("/console/api/variables/{key}", c.handleVariableUpdate)
			pr.Post("/console/api/variables/{key}/delete", c.handleVariableDelete)
		}
		// W4-E1/E2: environments + API keys management.
		pr.Get("/console/settings/environments", c.serveEnvironmentsPage)
		pr.Get("/console/settings/environments/new", c.serveEnvironmentNew)
		pr.Post("/console/settings/environments", c.handleEnvironmentCreate)
		pr.Post("/console/api/environments/{id}", c.handleEnvironmentUpdate)
		pr.Post("/console/api/environments/{id}/delete", c.handleEnvironmentDelete)
		pr.Get("/console/api/fragments/environments-list", c.serveFragmentEnvironmentsList)
		pr.Get("/console/settings/api-keys", c.serveAPIKeysPage)
		pr.Post("/console/settings/api-keys/regenerate", c.handleAPIKeyRegenerate)
		// W4-D1/D2: inbound domain management (#53).
		if c.deps.InboundDomains != nil {
			pr.Get("/console/domains", c.serveDomainsPage)
			pr.Get("/console/domains/new", c.serveDomainNew)
			pr.Get("/console/domains/{name}", c.serveDomainDetail)
			pr.Post("/console/domains", c.handleDomainCreate)
			pr.Post("/console/api/domains/{name}/verify", c.handleDomainVerify)
			pr.Post("/console/api/domains/{name}/delete", c.handleDomainDelete)
			pr.Get("/console/api/fragments/domains-list", c.serveFragmentDomainsList)
		}
		// #106: read-only chat inbound status page (Telegram long-poll / Discord
		// gateway). Env-driven config — no mutation routes.
		if c.deps.ChatStatus != nil {
			pr.Get("/console/chat", c.serveChatPage)
		}
		// Inbound email: read-only config banner + received-thread activity log.
		// Gated on both the thread store and the inbound-domain service being
		// wired. No detail route, no mutations (read-only).
		if c.deps.EmailThreads != nil && c.deps.InboundDomains != nil {
			pr.Get("/console/inbound-email", c.serveInboundEmailPage)
			pr.Get("/console/api/fragments/inbound-email-list", c.serveFragmentInboundEmailList)
		}
		// W3: agents management + conversation console. The literal `new`
		// segment is registered before the {identifier} wildcard so chi never
		// matches "new" as an identifier.
		pr.Get("/console/agents", c.serveAgentsPage)
		pr.Get("/console/agents/new", c.serveAgentNew)
		pr.Get("/console/agents/{identifier}", c.serveAgentDetailPage)
		pr.Get("/console/agents/{identifier}/tabs/overview", c.serveAgentDetailPage)
		pr.Get("/console/agents/{identifier}/tabs/channels", c.serveAgentDetailPage)
		pr.Get("/console/agents/{identifier}/tabs/conversations", c.serveAgentDetailPage)
		pr.Get("/console/agents/{identifier}/conversations/{id}", c.serveConversationPage)
		pr.Post("/console/agents", c.handleAgentCreate)
		pr.Post("/console/agents/{identifier}/save", c.handleAgentSave)
		pr.Post("/console/agents/{identifier}/delete", c.handleAgentDelete)
		pr.Post("/console/agents/{identifier}/activate", c.handleAgentToggleActive)
		// #104: link/unlink channel integrations from the console (native form
		// POST → 303 back to the channels tab). Thin wrappers over the agents
		// store Links service the /v1 API also uses.
		pr.Post("/console/agents/{identifier}/channels", c.handleAgentChannelLink)
		pr.Post("/console/agents/{identifier}/channels/{integrationID}/unlink", c.handleAgentChannelUnlink)
		pr.Post("/console/agents/{identifier}/conversations/{id}/compose", c.handleConversationCompose)
		pr.Get("/console/api/fragments/agents-list", c.serveFragmentAgentsList)
		pr.Get("/console/api/fragments/agent-conversations", c.serveFragmentAgentConversations)
		// W4-A5: global conversations list (cross-agent inbox, #46).
		pr.Get("/console/conversations", c.serveConversationsPage)
		pr.Get("/console/api/fragments/global-conversations", c.serveFragmentGlobalConversations)
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
