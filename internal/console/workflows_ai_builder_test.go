package console

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
)

// aiToolStreamMsg builds one KindTool build Event carrying a tool name + call id
// (aiStreamMsg omits both). The copilot relay keys a chip on toolCallId.
func aiToolStreamMsg(t *testing.T, toolName, toolCallID string, draft *aibuilder.WorkflowDraft) []byte {
	t.Helper()
	m := map[string]any{"kind": "tool", "toolName": toolName, "toolCallId": toolCallID}
	if draft != nil {
		m["draft"] = draft
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal tool event: %v", err)
	}
	return b
}

// TestAIBuilderView_RendersPanelGatedOnAIConfigured proves GET
// /console/workflows/ai renders the docked copilot panel + canvas when the
// builder is wired, and a not-configured note when it is not.
func TestAIBuilderView_RendersPanelGatedOnAIConfigured(t *testing.T) {
	t.Run("configured renders the panel", func(t *testing.T) {
		tc := newTestConsole(t)
		tc.injectStreamingBuilder("run-x", nil, false, nil)
		cookie, _ := tc.login(t)

		rr := tc.get(t, "/console/workflows/ai", cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		body := html.UnescapeString(rr.Body.String())
		for _, want := range []string{
			`class="ai-copilot-panel"`,
			`id="ai-thread"`,
			`id="ai-copilot-form"`,
			`class="ai-copilot-starter"`,
			`"aiWidth":420`,
			`"aiStarted":false`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("configured view missing %q; body: %s", want, body)
			}
		}
	})

	t.Run("unconfigured renders the note", func(t *testing.T) {
		tc := newTestConsole(t) // aiBuilder stays nil
		cookie, _ := tc.login(t)

		rr := tc.get(t, "/console/workflows/ai", cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "not configured") {
			t.Errorf("unconfigured view missing the not-configured note; body: %s", body)
		}
		if strings.Contains(body, `id="ai-copilot-form"`) {
			t.Errorf("unconfigured view should not render the composer; body: %s", body)
		}
	})
}

// TestAIBuilderCopilot_CreateStreamsPerMessageChips drives the copilot create
// flow (view=copilot) and asserts the SSE relay patches a NEW assistant turn in
// #ai-thread: a user bubble, the per-message chip containers (id ai-msg-{msgID}-chips)
// keyed to the turn's message id, one chip per tool call (chip-{toolCallId}) with a
// state transition to done, the live canvas, and a $aiSessionID signal patch so
// follow-ups target the session.
func TestAIBuilderCopilot_CreateStreamsPerMessageChips(t *testing.T) {
	tc := newTestConsole(t)
	draft := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)},
		},
	}
	events := [][]byte{
		aiStreamMsg(t, "reasoning", "Planning the welcome flow.", draft, ""),
		aiToolStreamMsg(t, "setWorkflowMetadata", "tc-1", draft),
		aiToolStreamMsg(t, "addStep", "tc-2", draft),
		aiStreamMsg(t, "canvas", "", draft, ""),
		aiStreamMsg(t, "done", "Built a one-step welcome flow.", draft, ""),
	}
	tc.injectStreamingBuilder("run-1", draft, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome new users with an email"},
		"view":       {"copilot"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The session id is pushed to the client via a signal patch so follow-ups route.
	m := regexp.MustCompile(`"aiSessionID":"([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no aiSessionID signal patch in stream; body: %s", body)
	}
	sessID := m[1]

	// The user turn was recorded; its id keys the assistant turn's containers.
	msgs, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sessID)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("Messages(%s) = %+v, err %v", sessID, msgs, err)
	}
	turnID := msgs[0].ID // the user turn
	if msgs[0].Role != "user" || msgs[0].Content != "welcome new users with an email" {
		t.Fatalf("first message = %+v, want the user prompt", msgs[0])
	}

	for _, want := range []string{
		"welcome new users with an email",      // user bubble echoed
		`id="ai-msg-` + turnID + `-chips"`,     // per-message chip container
		`id="ai-msg-` + turnID + `-reasoning"`, // per-message reasoning container
		`id="ai-msg-` + turnID + `-actions"`,   // empty actions placeholder
		`id="chip-tc-1"`,                       // chip keyed on tool call id
		`id="chip-tc-2"`,
		`data-state="streaming"`,             // a chip rendered mid-stream
		`data-state="done"`,                  // and transitioned to done
		`data-component="ai-builder-canvas"`, // live canvas rendered
		"Welcome Flow",                       // the workflow taking shape (canvas name)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("copilot stream missing %q; body: %s", want, body)
		}
	}
}

// TestAIBuilderCopilot_FollowUpAppendsSecondTurn proves a follow-up message
// (view=copilot) to an existing session appends a SECOND assistant turn keyed to
// the new user message id — turns do not collide.
func TestAIBuilderCopilot_FollowUpAppendsSecondTurn(t *testing.T) {
	tc := newTestConsole(t)
	turn1 := aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)}},
	}
	sessID := tc.seedBuilderDraft(t, tc.scope, turn1)

	turn2 := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)},
			{StepID: "s2", Type: "sms", Name: "Nudge", ControlValues: json.RawMessage(`{"body":"psst"}`)},
		},
	}
	events := [][]byte{
		aiToolStreamMsg(t, "addStep", "tc-9", turn2),
		aiStreamMsg(t, "done", "Added an sms reminder.", turn2, ""),
	}
	tc.injectStreamingBuilder("run-2", turn2, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt":     {"add an sms reminder"},
		"view":       {"copilot"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	msgs, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	// The just-appended user turn keys the new assistant turn's containers.
	var userTurnID string
	for _, msg := range msgs {
		if msg.Role == "user" && msg.Content == "add an sms reminder" {
			userTurnID = msg.ID
		}
	}
	if userTurnID == "" {
		t.Fatalf("follow-up user turn not recorded; msgs: %+v", msgs)
	}
	for _, want := range []string{
		"add an sms reminder",
		`id="ai-msg-` + userTurnID + `-chips"`,
		`id="chip-tc-9"`,
		`data-component="ai-builder-canvas"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("follow-up stream missing %q; body: %s", want, body)
		}
	}
}

// TestAIBuilderCopilot_CreateDoesNotEmitStaleRemovalNoise proves a brand-new
// turn (create or follow-up — never a retry) does NOT emit a remove-mode
// PatchElements against #ai-turn-{id}/-u. That defensive removal exists only
// to drop a retried turn's stale DOM before re-streaming; firing it
// unconditionally on every turn targets selectors that have never existed in
// the DOM yet, which is exactly what trips the Datastar client's
// PatchElementsNoTargetsFound console warning on every single turn.
func TestAIBuilderCopilot_CreateDoesNotEmitStaleRemovalNoise(t *testing.T) {
	tc := newTestConsole(t)
	draft := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)}},
	}
	events := [][]byte{aiStreamMsg(t, "done", "Built a one-step welcome flow.", draft, "")}
	tc.injectStreamingBuilder("run-noscroll", draft, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome new users with an email"},
		"view":       {"copilot"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "mode remove") {
		t.Errorf("a brand-new turn emitted a remove-mode patch (stale-DOM cleanup belongs to retry only); body: %s", body)
	}
}

// TestAIBuilderView_ReplaysPriorTurns proves resuming a session (?session=)
// renders its draft into the canvas and replays its stored turns into the thread.
func TestAIBuilderView_ReplaysPriorTurns(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)

	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps:             []domain.Step{{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)}},
	})
	ctx := context.Background()
	if _, err := tc.st.BuilderSessions().AppendMessage(ctx, tc.scope, sessID, "user", "build a welcome flow", ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if _, err := tc.st.BuilderSessions().AppendMessage(ctx, tc.scope, sessID, "assistant", "Added a welcome email.", ""); err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/workflows/ai?session="+sessID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := html.UnescapeString(rr.Body.String())
	for _, want := range []string{
		"build a welcome flow",               // replayed user turn
		"Added a welcome email.",             // replayed assistant turn
		`data-component="ai-builder-canvas"`, // canvas seeded from the draft
		`"aiStarted":true`,                   // empty state stays hidden on resume
		`"aiSessionID":"` + sessID + `"`,     // follow-ups target this session
	} {
		if !strings.Contains(body, want) {
			t.Errorf("resumed view missing %q; body: %s", want, body)
		}
	}
}

// TestAIBuilder_DomainScopeIsolation proves a session from another scope is not
// replayable (renders a fresh, sessionless panel rather than leaking).
func TestAIBuilder_DomainScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	other := domain.Scope{OrgID: "org-other", EnvID: "env-other"}
	sessID := tc.seedBuilderDraft(t, other, aibuilder.WorkflowDraft{Name: "Secret", TriggerIdentifier: "secret"})

	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/workflows/ai?session="+sessID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "Secret") {
		t.Errorf("cross-scope session leaked into the builder view; body: %s", body)
	}
	if strings.Contains(body, `"aiSessionID":"`+sessID+`"`) {
		t.Errorf("cross-scope session id bound to the panel; body: %s", body)
	}
}
