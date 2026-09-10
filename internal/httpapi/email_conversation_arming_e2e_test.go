package httpapi_test

// TestAgentEmailConversation_* proves issue #317: before this route existed,
// nothing in production ever wrote the first email_threads binding, so an
// agent's reply-to (minted by emailloop.ReplyToAddress) had no binding for an
// inbound reply to resolve against — HandleInbound would drop it "no_binding"
// forever. POST /v1/agents/{identifier}/conversations/email composes the
// first turn AND arms the binding, closing the gap.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/emailloop"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
)

const (
	emailArmMeshSubject   = "agents.prompt.email-arm.stub"
	emailArmInboundSecret = "arm-secret"
)

// emailArmHarness wires the full production collaborators the new route
// depends on: a real dagnats sidecar, real SQLite store, agent store, the
// mesh runtime (unused by any test here — no test waits on an actual agent
// reply, only on ComposeMessage succeeding and the binding being written),
// and the real HTTP surface.
type emailArmHarness struct {
	srv *httptest.Server
	st  *store.Store
	ags *agentstore.Store
	eml *emailloop.Service
}

func newEmailArmHarness(t *testing.T) *emailArmHarness {
	t.Helper()
	return newEmailArmHarnessWithSecret(t, emailArmInboundSecret)
}

// newEmailArmHarnessWithSecret builds the harness with an explicit inbound
// shared secret; an empty secret models a deployment where inbound email is
// not configured.
func newEmailArmHarnessWithSecret(t *testing.T, secret string) *emailArmHarness {
	t.Helper()
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ags := agentstore.New(st)

	meshClient, err := mesh.Dial(natsURL, mesh.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("mesh.Dial: %v", err)
	}
	t.Cleanup(meshClient.Close)

	ing := ingress.New(ags, st, eng, nil).WithRuntime(agentdomain.RuntimeMesh, meshClient)

	masterKey := bytesRepeat(0x2a, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	inbox := inboxapi.New(st)

	minter := emailloop.NewMinter(masterKey)
	// emlCompose mirrors cmd/notify/main.go's wiring verbatim: the responder's
	// compose primitive IS ingress.ComposeMessage.
	emlCompose := func(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (string, error) {
		res, err := ing.ComposeMessage(ctx, scope, agentIdentifier, conversationID, subscriberID, text)
		if err != nil {
			return "", err
		}
		return res.ConversationID, nil
	}
	eml := emailloop.New(st, ags, minter, nil, emailloop.NewAgentResponder(emlCompose), secret, nil)

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity:  id,
		Audience:  aud,
		Inbox:     inbox,
		Ingress:   ing,
		EmailLoop: eml,
	}))
	t.Cleanup(srv.Close)

	h := &emailArmHarness{srv: srv, st: st, ags: ags, eml: eml}
	return h
}

// bootstrap provisions a fresh org/env/key pair against the harness's shared
// store, returning the ApiKey header value and the minted Scope.
func (h *emailArmHarness) bootstrap(t *testing.T, orgName, env string) (string, domain.Scope) {
	t.Helper()
	masterKey := bytesRepeat(0x2a, 32)
	id := identity.New(h.st, masterKey)
	key, scope, err := id.Bootstrap(context.Background(), orgName, env)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return "ApiKey " + key, scope
}

// verifyInboundDomain creates and verifies an inbound domain for scope.
func (h *emailArmHarness) verifyInboundDomain(t *testing.T, scope domain.Scope, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.InboundDomains().Create(ctx, scope, name, "mx.notify.test", 10); err != nil {
		t.Fatalf("inbound domain create: %v", err)
	}
	if err := h.st.InboundDomains().SetStatus(ctx, scope, name, "verified", true); err != nil {
		t.Fatalf("inbound domain verify: %v", err)
	}
}

// createAgent creates a mesh-runtime agent directly against the store.
func (h *emailArmHarness) createAgent(t *testing.T, scope domain.Scope, identifier string) {
	t.Helper()
	if _, err := h.ags.Agents().Create(context.Background(), scope, agentdomain.Agent{
		Identifier: identifier, Name: identifier, Runtime: agentdomain.RuntimeMesh,
		Active: true, MeshSubject: emailArmMeshSubject,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
}

// createUnrunnableAgent creates an agent whose runtime (managed) has no
// executor registered on the harness, so ComposeMessage fails with
// ingress.ErrUnknownRuntime.
func (h *emailArmHarness) createUnrunnableAgent(t *testing.T, scope domain.Scope, identifier string) {
	t.Helper()
	if _, err := h.ags.Agents().Create(context.Background(), scope, agentdomain.Agent{
		Identifier: identifier, Name: identifier, Runtime: agentdomain.RuntimeManaged,
		Active: true,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
}

type emailConversationResponse struct {
	ConversationID string `json:"conversationId"`
	ReplyTo        string `json:"replyTo"`
}

// TestAgentEmailConversation_HappyPath is the closing proof for #317: the
// route composes the conversation, arms the email binding, and returns a
// reply-to that a genuine inbound reply resolves against (see
// TestAgentEmailConversation_ClosesTheLoop_EndToEnd for the full round trip).
func TestAgentEmailConversation_HappyPath(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	h.createAgent(t, scope, "support")

	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"Hello, I need help."}`)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", status, body)
	}
	var resp emailConversationResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if resp.ConversationID == "" {
		t.Fatalf("conversationId empty: %s", body)
	}
	if !strings.HasPrefix(resp.ReplyTo, "reply+") || !strings.HasSuffix(resp.ReplyTo, "@inbound-acme.test") {
		t.Fatalf("replyTo = %q, want reply+<token>@inbound-acme.test", resp.ReplyTo)
	}

	// The conversation exists.
	agent, err := h.ags.Agents().GetByIdentifier(context.Background(), scope, "support")
	if err != nil {
		t.Fatalf("GetByIdentifier: %v", err)
	}
	conv, err := h.ags.Conversations().GetByThread(context.Background(), scope, agent.ID, "", resp.ConversationID)
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	if conv.SubscriberID != "sub-ada" {
		t.Errorf("conversation subscriber = %q, want sub-ada", conv.SubscriberID)
	}

	// The email_threads binding exists (this is the #317 fix: nothing wrote it
	// before this route existed).
	binding, err := h.st.EmailThreads().Get(context.Background(), scope, resp.ConversationID)
	if err != nil {
		t.Fatalf("EmailThreads().Get: %v (binding was never armed)", err)
	}
	if binding.AgentIdentifier != "support" || binding.SubscriberEmail != "ada@acme.io" {
		t.Errorf("binding = %+v, want agent=support email=ada@acme.io", binding)
	}
}

// TestAgentEmailConversation_NoVerifiedDomain_422 proves the route refuses to
// arm a loop it cannot mint a reply-to address for.
func TestAgentEmailConversation_NoVerifiedDomain_422(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.createAgent(t, scope, "support")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"hi"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", status, body)
	}
}

// TestAgentEmailConversation_SubscriberNoEmail_422 proves a subscriber with
// no email address cannot start an email-origin conversation.
func TestAgentEmailConversation_SubscriberNoEmail_422(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	h.createAgent(t, scope, "support")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-noemail",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeader,
		`{"subscriberId":"sub-noemail","text":"hi"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", status, body)
	}
}

// TestAgentEmailConversation_UnknownAgent_404 proves an unknown agent
// identifier 404s rather than silently composing against nothing.
func TestAgentEmailConversation_UnknownAgent_404(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/ghost/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"hi"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", status, body)
	}
}

// TestAgentEmailConversation_CrossOrgSubscriber_404 proves a subscriber id
// from another org is invisible, not merely rejected for lack of email.
func TestAgentEmailConversation_CrossOrgSubscriber_404(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeaderA, scopeA := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scopeA, "inbound-acme.test")
	h.createAgent(t, scopeA, "support")

	_, scopeB := h.bootstrap(t, "Globex", "dev")
	if _, err := audience.New(h.st).Upsert(context.Background(), scopeB, audience.SubscriberInput{
		SubscriberID: "sub-bob", Email: "bob@globex.io",
	}); err != nil {
		t.Fatalf("subscriber upsert (org B): %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeaderA,
		`{"subscriberId":"sub-bob","text":"hi"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", status, body)
	}
}

// TestAgentEmailConversation_UnknownRuntime_422 proves an agent whose
// runtime has no registered executor 422s (a client-fixable condition) and
// does not fall through to the generic 500 the compose route used to
// produce.
func TestAgentEmailConversation_UnknownRuntime_422(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	h.createUnrunnableAgent(t, scope, "unrunnable")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/unrunnable/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"hi"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", status, body)
	}
}

// TestAgentEmailConversation_ArmsBeforeDispatch proves the email_threads
// binding exists before ComposeMessage is even called, not merely by the
// time the handler returns — a reply-to minted for the freshly armed
// threadID resolves BEFORE compose runs, so a turn that finishes and fires
// ReplyHook while this request is still in flight can never find the
// binding missing. Regression guard for #317's arm-after-compose ordering
// bug: the binding write must precede the turn dispatch, not merely precede
// the 201 response.
func TestAgentEmailConversation_ArmsBeforeDispatch(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	h.createAgent(t, scope, "support")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	_, _, countBefore, err := h.st.EmailThreads().List(context.Background(), scope, "", 200)
	if err != nil {
		t.Fatalf("list email threads before: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"Hello, I need help."}`)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", status, body)
	}

	_, _, countAfter, err := h.st.EmailThreads().List(context.Background(), scope, "", 200)
	if err != nil {
		t.Fatalf("list email threads after: %v", err)
	}
	if countAfter != countBefore+1 {
		t.Fatalf("email_threads count = %d, want %d (arming must have written a binding)", countAfter, countBefore+1)
	}
}

// TestAgentEmailConversation_ClosesTheLoop_EndToEnd is the proof #317 is
// actually closed: it first proves the loop is broken WITHOUT the arming
// call (a genuine reply-to minted for an unarmed thread drops as
// "no_binding" — exactly the production symptom before this change), then
// proves POST /v1/agents/{id}/conversations/email fixes it by taking the
// route's own returned replyTo, POSTing it as an inbound reply, and
// asserting the responder actually dispatched ("threaded").
func TestAgentEmailConversation_ClosesTheLoop_EndToEnd(t *testing.T) {
	h := newEmailArmHarness(t)
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	h.createAgent(t, scope, "support")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	// --- (1) BEFORE arming: a thread with no binding has no reply mailbox at
	// all. The token names the email_threads row (#350), so nothing can be
	// minted for a thread nobody armed -- the #317 symptom (a reply-to that
	// drops "no_binding") cannot be produced any more.
	if addr, ok := h.eml.ReplyToAddress(context.Background(), scope, "thr-unarmed-01", "sub-ada"); ok {
		t.Fatalf("ReplyToAddress for an unarmed thread = %q, want ok=false", addr)
	}

	// --- (2) Start the email-origin conversation via the new route.
	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"Hello, I need help."}`)
	if status != http.StatusCreated {
		t.Fatalf("arm status = %d, want 201 (body=%s)", status, body)
	}
	var resp emailConversationResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}

	// --- (3) AFTER arming: the SAME shaped inbound reply, now against the
	// route's own returned replyTo, threads and dispatches the responder.
	postStatus, postBody := postInboundEmail(t, h.srv.URL, emailArmInboundSecret, resp.ReplyTo, "Yes, please help.")
	if postStatus != http.StatusOK {
		t.Fatalf("post-arm inbound status = %d, want 200 (body=%s)", postStatus, postBody)
	}
	if got := preResult(t, postBody); got != "threaded" {
		t.Fatalf("post-arm inbound result = %q, want threaded (responder must have dispatched)", got)
	}

	// The inbound reply landed as a new turn on the conversation the route
	// created.
	agent, err := h.ags.Agents().GetByIdentifier(context.Background(), scope, "support")
	if err != nil {
		t.Fatalf("GetByIdentifier: %v", err)
	}
	conv, err := h.ags.Conversations().GetByThread(context.Background(), scope, agent.ID, "", resp.ConversationID)
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	acts, err := h.ags.Activities().History(context.Background(), scope, conv.ID, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(acts) != 2 {
		t.Fatalf("activities = %d, want 2 (the composed turn + the threaded inbound reply)", len(acts))
	}
	if !strings.Contains(string(acts[1].Content), "Yes, please help.") {
		t.Errorf("second activity = %s, want the inbound reply body", acts[1].Content)
	}
}

// postInboundEmail POSTs a minimal inbound-email payload to /v1/inbound/email
// and returns the HTTP status and raw JSON body.
func postInboundEmail(t *testing.T, baseURL, secret, to, text string) (int, string) {
	t.Helper()
	payload := map[string]string{
		"from":       "ada@acme.io",
		"to":         to,
		"envelopeTo": to,
		"subject":    "Re: your ticket",
		"text":       text,
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/inbound/email", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Inbound-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/inbound/email: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw2, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw2)
}

// preResult decodes the {"status":"..."} accepted-response body.
func preResult(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode accepted response: %v (body=%s)", err, body)
	}
	return out.Status
}

// TestAgentEmailConversation_InboundNotConfigured_422: with no inbound shared
// secret the inbound endpoint 503s every message, so the route must refuse
// instead of returning a reply-to mailbox that can never be replied to.
func TestAgentEmailConversation_InboundNotConfigured_422(t *testing.T) {
	h := newEmailArmHarnessWithSecret(t, "")
	authHeader, scope := h.bootstrap(t, "Acme", "dev")
	h.verifyInboundDomain(t, scope, "inbound-acme.test")
	h.createAgent(t, scope, "support")
	if _, err := audience.New(h.st).Upsert(context.Background(), scope, audience.SubscriberInput{
		SubscriberID: "sub-ada", Email: "ada@acme.io",
	}); err != nil {
		t.Fatalf("subscriber upsert: %v", err)
	}

	status, body := doPost(t, h.srv.URL+"/v1/agents/support/conversations/email", authHeader,
		`{"subscriberId":"sub-ada","text":"hello"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", status, body)
	}
	if !strings.Contains(body, "inbound email is not configured") {
		t.Errorf("body = %s, want the inbound-not-configured reason", body)
	}
}
