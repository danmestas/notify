package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
		// The durable finish step persists the final draft AND flips the session to
		// the terminal Ready status; the stub mirrors that so the keep happy path
		// (which requires Ready) works.
		if err := s.repo.UpdateDraft(ctx, scope, sessionID, dj, store.BuilderSessionReady); err != nil {
			return "", err
		}
	}
	return s.runID, nil
}

func (s *stubAIBuilder) RunFailed(_ context.Context, _ string) (bool, error) {
	return s.failed, nil
}

// stubBuildStream replays a scripted sequence of raw build-stream events to the
// subscriber's handler in a goroutine, so a copilot turn drains them through its
// select loop exactly like a live NATS subscription. unsubscribe is a no-op (the
// goroutine finishes once the script is exhausted).
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
// draft (what the durable run leaves behind), returning its id. Keep/message
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
	if err := tc.st.BuilderSessions().UpdateDraft(context.Background(), scope, sess.ID, dj, store.BuilderSessionReady); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	return sess.ID
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
