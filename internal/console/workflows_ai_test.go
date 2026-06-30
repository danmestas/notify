package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// blockingClient is an aibuilder.Client that blocks until its context is
// cancelled, then returns an error wrapping ctx.Err(). It exercises the
// generate handler's wall-clock timeout guard without an API key.
type blockingClient struct{}

func (blockingClient) Message(ctx context.Context, _ aibuilder.MessageRequest) (aibuilder.MessageResponse, error) {
	<-ctx.Done()
	return aibuilder.MessageResponse{}, fmt.Errorf("blocked: %w", ctx.Err())
}

// scriptClient is a scripted aibuilder.Client: it returns the next canned
// response on each Message call, repeating the last one once exhausted (so the
// builder's terminal end_turn is always available).
type scriptClient struct {
	resp []aibuilder.MessageResponse
	n    int
}

func (c *scriptClient) Message(_ context.Context, _ aibuilder.MessageRequest) (aibuilder.MessageResponse, error) {
	r := c.resp[c.n]
	if c.n < len(c.resp)-1 {
		c.n++
	}
	return r, nil
}

func aiTool(id, name, input string) aibuilder.ToolCall {
	return aibuilder.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

// injectScriptedBuilder wires a real aibuilder.Builder backed by a scripted
// client into the console, so the whole engine (gating + draft mutation +
// validation) runs without an API key.
func (tc *testConsole) injectScriptedBuilder(resp []aibuilder.MessageResponse) {
	tc.c.aiBuilder = aibuilder.NewBuilder(&scriptClient{resp: resp}, tc.deps.Integrations)
}

// TestAIGenerate_ProducesDraftPreviewAndSession drives the generate endpoint
// with a scripted builder and asserts: a preview fragment naming the workflow
// and its steps, plus a persisted session carrying the validated draft.
func TestAIGenerate_ProducesDraftPreviewAndSession(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	tc.injectScriptedBuilder([]aibuilder.MessageResponse{
		{StopReason: "tool_use", Text: "Adding a welcome email.", ToolCalls: []aibuilder.ToolCall{
			aiTool("t1", "setWorkflowMetadata", `{"name":"Welcome Flow"}`),
			aiTool("t2", "addStep", `{"type":"email","name":"Greeting"}`),
		}},
		{StopReason: "end_turn", Text: "Built a one-step welcome flow."},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome new users by email"},
		"csrf_token": {csrf},
	}, cookie)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Welcome Flow") {
		t.Errorf("preview missing workflow name; body: %s", body)
	}
	if !strings.Contains(body, "Email") {
		t.Errorf("preview missing email step label; body: %s", body)
	}
	if !strings.Contains(body, "welcome-flow") {
		t.Errorf("preview missing slugified trigger; body: %s", body)
	}

	// Exactly one session persisted, holding the validated draft.
	sess := onlyBuilderSession(t, tc)
	var draft aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &draft); err != nil {
		t.Fatalf("unmarshal persisted draft: %v", err)
	}
	if draft.Name != "Welcome Flow" || draft.TriggerIdentifier != "welcome-flow" {
		t.Fatalf("persisted draft = %+v", draft)
	}
	if len(draft.Steps) != 1 || draft.Steps[0].Type != "email" {
		t.Fatalf("persisted draft steps = %+v", draft.Steps)
	}
	// The reasoning text was recorded in the message history.
	msgs, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) == 0 {
		t.Error("expected at least one recorded builder message")
	}
}

// TestAIConfirm_CreatesAndPublishesWorkflow confirms a generated draft and
// asserts the workflow now exists and the response redirects to its detail page.
func TestAIConfirm_CreatesAndPublishesWorkflow(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	tc.injectScriptedBuilder([]aibuilder.MessageResponse{
		{StopReason: "tool_use", ToolCalls: []aibuilder.ToolCall{
			aiTool("t1", "setWorkflowMetadata", `{"name":"Welcome Flow"}`),
			aiTool("t2", "addStep", `{"type":"email"}`),
		}},
		{StopReason: "end_turn", Text: "done"},
	})
	cookie, csrf := tc.login(t)

	tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome email"},
		"csrf_token": {csrf},
	}, cookie)
	sess := onlyBuilderSession(t, tc)

	rr := tc.postForm(t, "/console/api/workflows/ai-confirm/"+sess.ID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("confirm status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/console/workflows/welcome-flow" {
		t.Fatalf("redirect Location = %q, want /console/workflows/welcome-flow", loc)
	}

	// The workflow now exists in the store.
	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome-flow")
	if err != nil {
		t.Fatalf("created workflow not found: %v", err)
	}
	if wf.Name != "Welcome Flow" {
		t.Errorf("created workflow name = %q", wf.Name)
	}

	// The session is marked confirmed.
	got, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sess.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status != store.BuilderSessionConfirmed {
		t.Errorf("session status = %q, want confirmed", got.Status)
	}
}

// TestAIGenerate_TimeoutRendersInlineMessage proves the generate handler caps
// the synchronous builder run with a wall-clock timeout: a wedged model yields
// an inline "timed out" message (HTTP 200), never a hung request or 500.
func TestAIGenerate_TimeoutRendersInlineMessage(t *testing.T) {
	tc := newTestConsole(t)
	tc.c.aiBuilder = aibuilder.NewBuilder(&blockingClient{}, tc.deps.Integrations)
	tc.c.aiGenerateTimeoutOverride = 5 * time.Millisecond
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"build me something slow"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "timed out") {
		t.Errorf("expected an inline timeout message; body: %s", rr.Body.String())
	}
}

// TestAIConfirm_IdempotentDoubleSubmit proves a second confirm of the same
// session is a graceful no-op: HTTP 200 with an "already" message, not a 500
// from a spurious identifier conflict, and no duplicate workflow.
func TestAIConfirm_IdempotentDoubleSubmit(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	tc.injectScriptedBuilder([]aibuilder.MessageResponse{
		{StopReason: "tool_use", ToolCalls: []aibuilder.ToolCall{
			aiTool("t1", "setWorkflowMetadata", `{"name":"Welcome Flow"}`),
			aiTool("t2", "addStep", `{"type":"email"}`),
		}},
		{StopReason: "end_turn", Text: "done"},
	})
	cookie, csrf := tc.login(t)

	tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome email"},
		"csrf_token": {csrf},
	}, cookie)
	sess := onlyBuilderSession(t, tc)

	first := tc.postForm(t, "/console/api/workflows/ai-confirm/"+sess.ID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first confirm status = %d, want 303 (body: %s)", first.Code, first.Body.String())
	}

	second := tc.postForm(t, "/console/api/workflows/ai-confirm/"+sess.ID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if second.Code != http.StatusOK {
		t.Fatalf("second confirm status = %d, want 200 (body: %s)", second.Code, second.Body.String())
	}
	if !strings.Contains(strings.ToLower(second.Body.String()), "already") {
		t.Errorf("expected an 'already confirmed' message; body: %s", second.Body.String())
	}

	// The workflow exists exactly once (no duplicate create on the re-submit).
	if _, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome-flow"); err != nil {
		t.Fatalf("created workflow not found: %v", err)
	}
	// Still exactly one builder session.
	_ = onlyBuilderSession(t, tc)
}

// TestAIConfirm_ScopeIsolated proves a session belonging to another environment
// cannot be confirmed by the logged-in operator (no workflow leaks across the
// tenancy boundary).
func TestAIConfirm_ScopeIsolated(t *testing.T) {
	tc := newTestConsole(t)
	alt := tc.altScope(t)

	// A fully-formed draft created under the alt scope.
	other, err := tc.st.BuilderSessions().Create(context.Background(), alt, "secret flow")
	if err != nil {
		t.Fatalf("create alt session: %v", err)
	}
	draft := aibuilder.WorkflowDraft{
		Name:              "Secret",
		TriggerIdentifier: "secret",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	}
	dj, _ := json.Marshal(draft)
	if err := tc.st.BuilderSessions().UpdateDraft(context.Background(), alt, other.ID, dj, store.BuilderSessionDraft); err != nil {
		t.Fatalf("update alt draft: %v", err)
	}

	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/api/workflows/ai-confirm/"+other.ID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-scope confirm status = %d, want 404", rr.Code)
	}
	// No workflow was created in the operator's scope.
	if _, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "secret"); err == nil {
		t.Fatal("cross-scope confirm leaked a workflow into the operator's scope")
	}
}

// TestAIGenerate_Unconfigured_GracefulMessage asserts the generate endpoint
// returns a clear not-configured message (never a panic / 500) when no key is
// wired (aiBuilder is nil).
func TestAIGenerate_Unconfigured_GracefulMessage(t *testing.T) {
	tc := newTestConsole(t) // no AnthropicKey -> aiBuilder nil
	if tc.c.aiBuilder != nil {
		t.Fatal("expected aiBuilder nil without a key")
	}
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"anything"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not configured") {
		t.Errorf("expected a not-configured message; body: %s", rr.Body.String())
	}
}

// TestAIGenerate_GatingReflectsConfiguredChannels proves the builder only adds
// steps for channels with an active integration: email is offered (active),
// sms is rejected, so the persisted draft holds only the email step.
func TestAIGenerate_GatingReflectsConfiguredChannels(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	// No sms integration: an sms add must be rejected at runtime.
	tc.injectScriptedBuilder([]aibuilder.MessageResponse{
		{StopReason: "tool_use", ToolCalls: []aibuilder.ToolCall{
			aiTool("t1", "setWorkflowMetadata", `{"name":"Mixed"}`),
			aiTool("t2", "addStep", `{"type":"email"}`),
			aiTool("t3", "addStep", `{"type":"sms"}`),
		}},
		{StopReason: "end_turn", Text: "done"},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"email and text the user"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	sess := onlyBuilderSession(t, tc)
	var draft aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &draft); err != nil {
		t.Fatalf("unmarshal draft: %v", err)
	}
	if len(draft.Steps) != 1 || draft.Steps[0].Type != "email" {
		t.Fatalf("gating not reflected: draft steps = %+v, want only email", draft.Steps)
	}
}

// TestWorkflowNewPage_GuidedNotConfiguredNote asserts the Guided tab renders a
// not-configured note when no key is wired.
func TestWorkflowNewPage_GuidedNotConfiguredNote(t *testing.T) {
	tc := newTestConsole(t) // aiBuilder nil
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/workflows/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not configured") {
		t.Errorf("expected the Guided not-configured note; body did not contain it")
	}
}

// onlyBuilderSession returns the single builder session in the test scope,
// failing if there is not exactly one. It reads the rows directly via the repo
// (there is no list method) by reaching through a fresh scoped query helper.
func onlyBuilderSession(t *testing.T, tc *testConsole) store.BuilderSession {
	t.Helper()
	ids := builderSessionIDs(t, tc)
	if len(ids) != 1 {
		t.Fatalf("expected exactly one builder session, got %d", len(ids))
	}
	sess, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, ids[0])
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	return sess
}

// builderSessionIDs reads the builder_sessions ids for the test scope straight
// from the temp SQLite file (the repo intentionally exposes no list method).
func builderSessionIDs(t *testing.T, tc *testConsole) []string {
	t.Helper()
	db, err := sql.Open("sqlite", tc.dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(
		`SELECT id FROM builder_sessions WHERE org_id = ? AND environment_id = ? ORDER BY id`,
		tc.scope.OrgID, tc.scope.EnvID,
	)
	if err != nil {
		t.Fatalf("query builder_sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}
