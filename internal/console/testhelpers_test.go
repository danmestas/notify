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

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/chatloop"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/contexts"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/streamsource"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/webhooksout"
)

// stubTrigger is a TriggerIngester stub for unit tests. It does not
// require a live NATS engine — it just records the last request and returns
// a configurable response.
type stubTrigger struct {
	resp trigger.TriggerResponse
	err  error
	last trigger.TriggerRequest
}

func (s *stubTrigger) Ingest(_ context.Context, _ domain.Scope, req trigger.TriggerRequest) (trigger.TriggerResponse, error) {
	s.last = req
	return s.resp, s.err
}

// newStubTrigger returns a stub that acknowledges every ingest with a fixed
// transaction id.
func newStubTrigger(txid string) *stubTrigger {
	if txid == "" {
		txid = "txn-test-stub"
	}
	return &stubTrigger{
		resp: trigger.TriggerResponse{
			Acknowledged:  true,
			Status:        "processed",
			TransactionID: txid,
		},
	}
}

// testConsole bundles everything a console test needs: the constructed
// Console (white-box access), its mounted handler, the bootstrap API key,
// and the minted Scope + store for seeding.
type testConsole struct {
	c          *Console
	h          http.Handler
	key        string
	scope      domain.Scope
	st         *store.Store
	agentStore *agentstore.Store
	composer   *stubComposer
	deps       Deps
	dbPath     string
}

// stubComposer is a console.ConversationComposer stub for unit tests: it
// records the last compose call and returns the conversationID echoed back,
// without a live engine.
type stubComposer struct {
	calls []composeCall
	err   error
}

type composeCall struct {
	agentIdentifier, conversationID, subscriberID, text string
}

func (s *stubComposer) ComposeMessage(_ context.Context, _ domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (string, error) {
	s.calls = append(s.calls, composeCall{agentIdentifier, conversationID, subscriberID, text})
	if s.err != nil {
		return "", s.err
	}
	return conversationID, nil
}

// telegramToken is a non-empty bot-token placeholder used by chat-status tests
// to mark a provider as configured.
const telegramToken = "tg-token"

// stubChatStatus is a console.ChatStatusProvider stub for the chat inbound
// status page tests (#106). It builds a real chatloop snapshot from the
// configured tokens + default agent against the test's live agents store, so
// the default-agent resolution (active / unresolved) is exercised end-to-end.
type stubChatStatus struct {
	core         *store.Store
	telegram     string
	discord      string
	defaultAgent string
}

func (s *stubChatStatus) Status(ctx context.Context, scope domain.Scope) chatloop.Snapshot {
	svc := chatloop.New(s.core, chatloop.NewAgentResponder(
		func(context.Context, domain.Scope, string, string, string, string) (string, error) { return "", nil },
	), s.telegram, s.discord, s.defaultAgent, nil)
	return svc.Status(ctx, scope)
}

// setChatStatus reconfigures the harness's chat status provider and re-mounts
// the console so the chat page reflects the new tokens / default agent.
func (tc *testConsole) setChatStatus(telegram, discord, defaultAgent string) {
	st, _ := tc.deps.ChatStatus.(*stubChatStatus)
	st.telegram = telegram
	st.discord = discord
	st.defaultAgent = defaultAgent
	tc.rebuild()
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
	ags := agentstore.New(st)
	composer := &stubComposer{}
	ev, err := envvars.New(st, masterKey)
	if err != nil {
		t.Fatalf("envvars.New: %v", err)
	}
	deps := Deps{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Identity:         id,
		Authoring:        authoring.New(st, nil),
		Analytics:        analytics.New(st, nil),
		WorkflowVersions: st.WorkflowVersions(),
		Notifications:    st.Notifications(),
		Messages:         st.Messages(),
		Integrations:     st.Integrations(),
		Connectivity:     conn,
		StreamSources:    streamsource.New(st, masterKey),
		Audience:         audience.New(st),
		Webhooks:         webhooksout.NewManagement(st),
		Contexts:         contexts.New(st),
		Layouts:          st.Layouts(),
		Translations:     st.Translations(),
		Environments:     st.Environments(),
		EnvVars:          ev,
		InboundDomains:   inbounddomains.New(st),
		EmailThreads:     st.EmailThreads(),
		Trigger:          newStubTrigger(""),
		Agents:           ags,
		Composer:         composer,
		MasterKey:        masterKey,
		Preferences:      preferences.New(st),
		TopicTriggers:    st.TopicTriggers(),
		ChatStatus:       &stubChatStatus{core: st},
		BuilderSessions:  st.BuilderSessions(),
	}
	c := New(deps)
	return &testConsole{
		c:          c,
		h:          c.Routes(),
		key:        key,
		scope:      scope,
		st:         st,
		agentStore: ags,
		composer:   composer,
		deps:       deps,
		dbPath:     dbPath,
	}
}

// seedAgent creates one agent row through the agents store under the test
// scope and returns it.
func (tc *testConsole) seedAgent(t *testing.T, a agentdomain.Agent) agentdomain.Agent {
	t.Helper()
	created, err := tc.agentStore.Agents().Create(context.Background(), tc.scope, a)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return created
}

// seedAgentInScope creates an agent under an explicit scope (scope-isolation
// tests).
func (tc *testConsole) seedAgentInScope(t *testing.T, scope domain.Scope, a agentdomain.Agent) agentdomain.Agent {
	t.Helper()
	created, err := tc.agentStore.Agents().Create(context.Background(), scope, a)
	if err != nil {
		t.Fatalf("seed agent in scope: %v", err)
	}
	return created
}

// seedConversation upserts one conversation thread for an agent and returns
// the stored row.
func (tc *testConsole) seedConversation(t *testing.T, c agentdomain.Conversation) agentdomain.Conversation {
	t.Helper()
	created, err := tc.agentStore.Conversations().Upsert(context.Background(), tc.scope, c)
	if err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return created
}

// seedActivity appends one activity to a conversation and returns it.
func (tc *testConsole) seedActivity(t *testing.T, a agentdomain.Activity) agentdomain.Activity {
	t.Helper()
	created, err := tc.agentStore.Activities().Append(context.Background(), tc.scope, a)
	if err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	return created
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

// seedWorkflowRaw inserts a workflow row DIRECTLY through the store repo,
// bypassing authoring.Create's trigger-identifier allowlist. Create now
// rejects URL-unsafe ids (H2), but render-layer escaping (XSS defense) must
// still hold for any bad id that could already exist in the DB — legacy
// rows, direct inserts, or a future ingest path. Tests that assert on
// escaping seed their malicious id through here so Create's validation
// doesn't (correctly) block the fixture from ever reaching the renderer.
func (tc *testConsole) seedWorkflowRaw(t *testing.T, name, trigger string) {
	t.Helper()
	_, err := tc.st.Workflows().Create(context.Background(), tc.scope, domain.Workflow{
		Name:              name,
		TriggerIdentifier: trigger,
		Active:            true,
		Steps:             json.RawMessage(`[{"stepId":"s1","type":"in_app","controlValues":{"body":"hi"}}]`),
	})
	if err != nil {
		t.Fatalf("seed workflow raw (%q): %v", trigger, err)
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

// seedWorkflowAndReturn creates one workflow and returns the domain.Workflow.
func (tc *testConsole) seedWorkflowAndReturn(t *testing.T, name, trigger string) domain.Workflow {
	t.Helper()
	wf, err := tc.deps.Authoring.Create(context.Background(), tc.scope, authoring.WorkflowInput{
		Name:              name,
		TriggerIdentifier: trigger,
		Steps: []domain.Step{{
			StepID:        "s1",
			Type:          "in_app",
			ControlValues: json.RawMessage(`{"body":"hi"}`),
		}},
	})
	if err != nil {
		t.Fatalf("seedWorkflowAndReturn: %v", err)
	}
	return wf
}

// seedNotificationInScope creates a notification row under an explicit scope.
func (tc *testConsole) seedNotificationInScope(t *testing.T, scope domain.Scope, n domain.Notification) domain.Notification {
	t.Helper()
	created, err := tc.st.Notifications().Create(context.Background(), scope, n)
	if err != nil {
		t.Fatalf("seed notification in scope: %v", err)
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
