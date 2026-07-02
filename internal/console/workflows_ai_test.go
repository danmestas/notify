package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// stubAIBuilder is a scripted aiBuildDispatcher (no engine, no API key). Dispatch
// records the session and, when persistDraft is set, writes it to the session —
// simulating the durable run's finish step (UpdateDraft) so the done-event path
// can load the session draft. RunFailed reports a configured terminal failure.
type stubAIBuilder struct {
	runID        string
	persistDraft *aibuilder.WorkflowDraft
	repo         store.BuilderSessionRepo
	failed       bool
}

func (s *stubAIBuilder) Dispatch(ctx context.Context, scope domain.Scope, sessionID string) (string, error) {
	if s.persistDraft != nil && s.repo != nil {
		dj, err := json.Marshal(s.persistDraft)
		if err != nil {
			return "", err
		}
		if err := s.repo.UpdateDraft(ctx, scope, sessionID, dj, store.BuilderSessionDraft); err != nil {
			return "", err
		}
	}
	return s.runID, nil
}

func (s *stubAIBuilder) RunFailed(_ context.Context, _ string) (bool, error) {
	return s.failed, nil
}

// stubBuildStream replays a scripted sequence of raw build-stream events to the
// subscriber's handler in a goroutine, so the ai-generate handler drains them
// through its select loop exactly like a live NATS subscription. unsubscribe is a
// no-op (the goroutine finishes once the script is exhausted).
type stubBuildStream struct {
	events [][]byte
}

func (s *stubBuildStream) SubscribeBuildStream(_ string, handler func([]byte)) (func() error, error) {
	go func() {
		for _, e := range s.events {
			handler(e)
		}
	}()
	return func() error { return nil }, nil
}

// aiStreamMsg builds one on-the-wire build Event (stream.<runID>.build payload).
func aiStreamMsg(t *testing.T, kind, text string, draft *aibuilder.WorkflowDraft, errMsg string) []byte {
	t.Helper()
	m := map[string]any{"kind": kind}
	if text != "" {
		m["text"] = text
	}
	if draft != nil {
		m["draft"] = draft
	}
	if errMsg != "" {
		m["error"] = errMsg
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal stream event: %v", err)
	}
	return b
}

// injectStreamingBuilder wires the scripted dispatcher + stream into the console.
func (tc *testConsole) injectStreamingBuilder(runID string, persist *aibuilder.WorkflowDraft, failed bool, events [][]byte) {
	tc.c.aiBuilder = &stubAIBuilder{
		runID:        runID,
		persistDraft: persist,
		repo:         tc.st.BuilderSessions(),
		failed:       failed,
	}
	tc.c.buildStream = &stubBuildStream{events: events}
}

// seedBuilderDraft creates a builder session under scope holding a validated
// draft (what the durable run leaves behind), returning its id. Confirm/discard
// tests start from this state instead of driving a generate run.
func (tc *testConsole) seedBuilderDraft(t *testing.T, scope domain.Scope, draft aibuilder.WorkflowDraft) string {
	t.Helper()
	sess, err := tc.st.BuilderSessions().Create(context.Background(), scope, "seed prompt")
	if err != nil {
		t.Fatalf("seed builder session: %v", err)
	}
	dj, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal seed draft: %v", err)
	}
	if err := tc.st.BuilderSessions().UpdateDraft(context.Background(), scope, sess.ID, dj, store.BuilderSessionDraft); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	return sess.ID
}

// TestAIGenerate_StreamsReasoningCanvasPreview drives the generate endpoint with
// a scripted dispatcher + build stream and asserts each region relays: reasoning
// lines append, the canvas re-renders from the draft snapshot, and the terminal
// done event renders the final preview (name + steps + Use/Discard). It also
// proves exactly one session is created carrying the run's draft.
func TestAIGenerate_StreamsReasoningCanvasPreview(t *testing.T) {
	tc := newTestConsole(t)
	draft := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{{
			StepID:        "s1",
			Type:          "email",
			Name:          "Greeting",
			ControlValues: json.RawMessage(`{"subject":"Hi there"}`),
		}},
	}
	events := [][]byte{
		aiStreamMsg(t, "reasoning", "Adding a welcome email.", draft, ""),
		aiStreamMsg(t, "canvas", "", draft, ""),
		aiStreamMsg(t, "done", "Built a one-step welcome flow.", draft, ""),
	}
	tc.injectStreamingBuilder("run-1", draft, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome new users by email"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Reasoning relayed into #ai-builder-reasoning.
	if !strings.Contains(body, "ai-reasoning-line") || !strings.Contains(body, "Adding a welcome email.") {
		t.Errorf("reasoning line not relayed; body: %s", body)
	}
	// Canvas re-rendered from the draft snapshot (distinct marker proves it).
	if !strings.Contains(body, `data-component="ai-builder-canvas"`) {
		t.Errorf("canvas fragment not relayed; body: %s", body)
	}
	if !strings.Contains(body, "Welcome Flow") || !strings.Contains(body, "Email") {
		t.Errorf("canvas missing workflow name or email step; body: %s", body)
	}
	// Final preview relayed on done with the Use/Discard action pair.
	if !strings.Contains(body, `data-component="ai-builder-preview"`) {
		t.Errorf("final preview not relayed; body: %s", body)
	}
	if !strings.Contains(body, "welcome-flow") || !strings.Contains(body, "Use this workflow") || !strings.Contains(body, "Discard") {
		t.Errorf("final preview missing trigger or use/discard; body: %s", body)
	}

	// On done, the canvas was cleared: the done handler emits an empty canvas
	// patch (no data-component="ai-builder-canvas"), so there must be at least
	// two occurrences of id="ai-builder-canvas" in the body — the live canvas
	// from the canvas event, and the empty clear emitted on the done event.
	if n := strings.Count(body, `id="ai-builder-canvas"`); n < 2 {
		t.Errorf("expected ≥2 canvas patches (live + clear-on-done), got %d; body: %s", n, body)
	}

	// Exactly one session persisted, holding the run's validated draft.
	sess := onlyBuilderSession(t, tc)
	var got aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &got); err != nil {
		t.Fatalf("unmarshal persisted draft: %v", err)
	}
	if got.Name != "Welcome Flow" || len(got.Steps) != 1 || got.Steps[0].Type != "email" {
		t.Fatalf("persisted draft = %+v", got)
	}
}

// TestAIGenerate_ErrorEventRendersInlineFailure proves a terminal error event on
// the stream resolves the request with an inline failure (HTTP 200), not a hung
// connection, a 500, or a preview.
func TestAIGenerate_ErrorEventRendersInlineFailure(t *testing.T) {
	tc := newTestConsole(t)
	events := [][]byte{
		aiStreamMsg(t, "reasoning", "Thinking it through.", nil, ""),
		aiStreamMsg(t, "error", "", nil, "model transport exploded"),
	}
	tc.injectStreamingBuilder("run-1", nil, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"build me something"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(strings.ToLower(body), "try again") {
		t.Errorf("expected an inline failure message; body: %s", body)
	}
	if strings.Contains(body, "Use this workflow") {
		t.Errorf("error path must not render a usable preview; body: %s", body)
	}
}

// TestAIGenerate_TimeoutRendersInlineFailure proves the handler caps the stream
// with an overall deadline: a run that never emits a terminal event (the
// iteration-cap shape) resolves inline rather than hanging. The run-status poll
// is held off so the deadline is the resolving path.
func TestAIGenerate_TimeoutRendersInlineFailure(t *testing.T) {
	tc := newTestConsole(t)
	events := [][]byte{aiStreamMsg(t, "reasoning", "Working on it.", nil, "")}
	tc.injectStreamingBuilder("run-1", nil, false, events)
	tc.c.aiGenerateTimeoutOverride = 20 * time.Millisecond
	tc.c.aiBuildPollOverride = 10 * time.Second
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"build me something slow"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "did not finish") {
		t.Errorf("expected an inline didn't-finish message; body: %s", rr.Body.String())
	}
}

// TestAIGenerate_FailedRunRendersInlineFailure proves the run-status poll
// resolves a run that failed without a terminal stream event (the iteration-cap
// path) promptly, rather than waiting out the full deadline.
func TestAIGenerate_FailedRunRendersInlineFailure(t *testing.T) {
	tc := newTestConsole(t)
	events := [][]byte{aiStreamMsg(t, "reasoning", "Working on it.", nil, "")}
	tc.injectStreamingBuilder("run-1", nil, true, events)
	tc.c.aiGenerateTimeoutOverride = 10 * time.Second
	tc.c.aiBuildPollOverride = 5 * time.Millisecond
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"build me something"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "did not finish") {
		t.Errorf("expected an inline didn't-finish message; body: %s", rr.Body.String())
	}
}

// TestAIGenerate_Unconfigured_GracefulMessage asserts the generate endpoint
// returns a clear not-configured message (never a panic / 500) when no builder is
// wired (aiBuilder is nil).
func TestAIGenerate_Unconfigured_GracefulMessage(t *testing.T) {
	tc := newTestConsole(t) // no AIBuilder injected -> aiBuilder nil
	if tc.c.aiBuilder != nil {
		t.Fatal("expected aiBuilder nil without a wired dispatcher")
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

// TestAIConfirm_CreatesAndPublishesWorkflow confirms a seeded draft and asserts
// the workflow now exists and the response redirects to its detail page.
func TestAIConfirm_CreatesAndPublishesWorkflow(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-confirm/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("confirm status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/console/workflows/welcome-flow" {
		t.Fatalf("redirect Location = %q, want /console/workflows/welcome-flow", loc)
	}

	wf, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome-flow")
	if err != nil {
		t.Fatalf("created workflow not found: %v", err)
	}
	if wf.Name != "Welcome Flow" {
		t.Errorf("created workflow name = %q", wf.Name)
	}

	got, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status != store.BuilderSessionConfirmed {
		t.Errorf("session status = %q, want confirmed", got.Status)
	}
}

// TestAIConfirm_IdempotentDoubleSubmit proves a second confirm of the same
// session is a graceful no-op: HTTP 200 with an "already" message, not a 500 from
// a spurious identifier conflict, and no duplicate workflow.
func TestAIConfirm_IdempotentDoubleSubmit(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})
	cookie, csrf := tc.login(t)

	first := tc.postForm(t, "/console/api/workflows/ai-confirm/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first confirm status = %d, want 303 (body: %s)", first.Code, first.Body.String())
	}

	second := tc.postForm(t, "/console/api/workflows/ai-confirm/"+sessID, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if second.Code != http.StatusOK {
		t.Fatalf("second confirm status = %d, want 200 (body: %s)", second.Code, second.Body.String())
	}
	if !strings.Contains(strings.ToLower(second.Body.String()), "already") {
		t.Errorf("expected an 'already confirmed' message; body: %s", second.Body.String())
	}

	if _, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "welcome-flow"); err != nil {
		t.Fatalf("created workflow not found: %v", err)
	}
	_ = onlyBuilderSession(t, tc)
}

// TestAIConfirm_ScopeIsolated proves a session belonging to another environment
// cannot be confirmed by the logged-in operator (no workflow leaks across the
// tenancy boundary).
func TestAIConfirm_ScopeIsolated(t *testing.T) {
	tc := newTestConsole(t)
	alt := tc.altScope(t)

	other := tc.seedBuilderDraft(t, alt, aibuilder.WorkflowDraft{
		Name:              "Secret",
		TriggerIdentifier: "secret",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{}`)}},
	})

	cookie, csrf := tc.login(t)
	rr := tc.postForm(t, "/console/api/workflows/ai-confirm/"+other, url.Values{
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-scope confirm status = %d, want 404", rr.Code)
	}
	if _, err := tc.deps.Authoring.GetBySlug(context.Background(), tc.scope, "secret"); err == nil {
		t.Fatal("cross-scope confirm leaked a workflow into the operator's scope")
	}
}

// TestWorkflowNewPage_GuidedNotConfiguredNote asserts the Guided tab renders a
// not-configured note when no builder is wired.
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
// failing if there is not exactly one.
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
