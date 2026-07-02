package console

// B5 tests: suggestion cards, rate limiting, prompt clamp, and streaming
// robustness (completed-but-terminal-dropped run resolves to the ready draft).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
)

// ─── 1. Suggestion cards ─────────────────────────────────────────────────────

// TestSuggestionCards_GuidedTabRendered asserts the Guided create tab (on the
// /console/workflows/new page) renders suggestion cards when the AI builder is
// configured. Each card must be a <button> element (keyboard-accessible), carry
// a data-on:click handler, and contain the suggestion text.
func TestSuggestionCards_GuidedTabRendered(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	// At least one suggestion card must be present.
	if !strings.Contains(body, "ai-builder-suggestion-card") {
		t.Errorf("Guided tab missing suggestion cards (class ai-builder-suggestion-card); body did not contain it")
	}
	// Must be real buttons (keyboard-accessible).
	if !strings.Contains(body, `<button`) {
		t.Errorf("suggestion cards are not <button> elements")
	}
	// Must carry a Datastar colon-form click handler.
	if !strings.Contains(body, "data-on:click") {
		t.Errorf("suggestion cards missing data-on:click; body snippet: %s", body[:min(500, len(body))])
	}
	// At least one suggestion text must appear (substring check avoids HTML-escaping issues).
	if !strings.Contains(body, "Welcome series") {
		t.Errorf("first suggestion text not found in Guided tab body")
	}
	// Handler must set the textarea value (prefix check for wf-ai-prompt).
	if !strings.Contains(body, "wf-ai-prompt") {
		t.Errorf("suggestion card click does not target the wf-ai-prompt textarea")
	}
}

// TestSuggestionCards_GuidedTabHiddenWhenUnconfigured asserts no suggestion
// cards appear on the Guided tab when the AI builder is not wired — cards are
// inside the {{if $page.AIConfigured}} guard.
func TestSuggestionCards_GuidedTabHiddenWhenUnconfigured(t *testing.T) {
	tc := newTestConsole(t) // aiBuilder nil
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/new", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "ai-builder-suggestion-card") {
		t.Errorf("suggestion cards must not appear when AI builder is not configured")
	}
}

// TestSuggestionCards_RefineTabRendered asserts the Refine-with-AI sidebar on
// the workflow detail page also renders suggestion cards when wired.
func TestSuggestionCards_RefineTabRendered(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/welcome", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	if !strings.Contains(body, "ai-builder-suggestion-card") {
		t.Errorf("Refine sidebar missing suggestion cards; body did not contain ai-builder-suggestion-card")
	}
	// The suggestion text may be HTML-escaped (e.g. ' → &#39;); check a
	// substring that is safe across HTML escaping.
	if !strings.Contains(body, "Add an SMS reminder") {
		t.Errorf("first refine suggestion text not found in detail page body")
	}
	// Refine cards must target the refine textarea.
	if !strings.Contains(body, "wf-ai-refine-prompt") {
		t.Errorf("refine suggestion card click does not target the wf-ai-refine-prompt textarea")
	}
}

// ─── 2. Rate limiting ────────────────────────────────────────────────────────

// TestAIRateLimit_ExceededGenerate asserts that the (N+1)th request within the
// rate-limit window from the same scope is rejected with an inline message and
// does NOT create a builder session or dispatch a run.
func TestAIRateLimit_ExceededGenerate(t *testing.T) {
	tc := newTestConsole(t)
	// Provide a done event so each allowed request resolves quickly.
	doneEvent := aiStreamMsg(t, "done", "", &aibuilder.WorkflowDraft{
		Name: "Flow", TriggerIdentifier: "flow",
	}, "")
	tc.injectStreamingBuilder("run-rl", nil, false, [][]byte{doneEvent})
	tc.c.aiGenerateTimeoutOverride = 5 * time.Second
	tc.c.aiBuildPollOverride = 5 * time.Second
	cookie, csrf := tc.login(t)

	// Burn through the window.
	now := time.Now()
	tc.c.aiLimiter.now = func() time.Time { return now }
	for i := 0; i < aiGenerateRateLimit; i++ {
		rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
			"prompt":     {"build me a flow"},
			"csrf_token": {csrf},
		}, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rr.Code)
		}
	}

	// The (N+1)th request must be rejected inline.
	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"build me one more"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("rate-limited status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(strings.ToLower(body), "generating a lot") {
		t.Errorf("expected rate-limit inline message; body: %s", body)
	}
	// The rejected request must not have dispatched a run (the stub still
	// reports the run counter for the allowed requests only).
	sessions := builderSessionIDs(t, tc)
	// Each allowed request creates one session; the rejected one must not.
	if len(sessions) != aiGenerateRateLimit {
		t.Errorf("sessions after rate limit = %d, want %d (the rejected request must not create a session)", len(sessions), aiGenerateRateLimit)
	}
}

// TestAIRateLimit_DifferentScopeUnaffected asserts the rate limiter is keyed
// by scope so one environment cannot exhaust another's budget.
func TestAIRateLimit_DifferentScopeUnaffected(t *testing.T) {
	tc := newTestConsole(t)
	doneEvent := aiStreamMsg(t, "done", "", &aibuilder.WorkflowDraft{
		Name: "Flow", TriggerIdentifier: "flow",
	}, "")
	tc.injectStreamingBuilder("run-rl2", nil, false, [][]byte{doneEvent})
	tc.c.aiGenerateTimeoutOverride = 5 * time.Second
	tc.c.aiBuildPollOverride = 5 * time.Second
	cookie, csrf := tc.login(t)

	// Fix clock so both scopes share the same window.
	now := time.Now()
	tc.c.aiLimiter.now = func() time.Time { return now }

	// Exhaust the primary scope's window.
	for i := 0; i < aiGenerateRateLimit; i++ {
		tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
			"prompt":     {"build me a flow"},
			"csrf_token": {csrf},
		}, cookie)
	}
	// The primary scope is now rate-limited.
	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"one more"},
		"csrf_token": {csrf},
	}, cookie)
	if !strings.Contains(strings.ToLower(rr.Body.String()), "generating a lot") {
		t.Errorf("primary scope should be rate-limited; body: %s", rr.Body.String())
	}

	// A different scope must NOT be rate-limited: allow it directly through
	// the limiter (the HTTP test uses the primary scope, so we test the
	// limiter's Allow method with the alt scope directly).
	alt := tc.altScope(t)
	if !tc.c.aiLimiter.Allow(alt) {
		t.Errorf("alt scope Allow() = false after sibling scope exhaustion; scopes must be independent")
	}
}

// TestAIRateLimit_WindowResets asserts that once the fixed window expires,
// the scope may send requests again.
func TestAIRateLimit_WindowResets(t *testing.T) {
	scope := domain.Scope{OrgID: "test-org", EnvID: "test-env"}
	var clk time.Time
	l := &aiRateLimiter{now: func() time.Time { return clk }}

	clk = time.Unix(0, 0)
	for i := 0; i < aiGenerateRateLimit; i++ {
		if !l.Allow(scope) {
			t.Fatalf("request %d unexpectedly denied before limit", i+1)
		}
	}
	if l.Allow(scope) {
		t.Fatal("request at limit should be denied")
	}

	// Advance past the window.
	clk = clk.Add(aiRateLimitWindow + time.Millisecond)
	if !l.Allow(scope) {
		t.Error("Allow() = false after window reset; expected first request of new window to be allowed")
	}
}

// TestAIPromptClamp_Generate asserts a prompt over 2000 chars is rejected with
// an inline message before any session is created.
func TestAIPromptClamp_Generate(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-clamp", nil, false, nil)
	cookie, csrf := tc.login(t)

	long := strings.Repeat("a", aiPromptMaxLen+1)
	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {long},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "too long") {
		t.Errorf("expected prompt-too-long message; body: %s", rr.Body.String())
	}
	if len(builderSessionIDs(t, tc)) != 0 {
		t.Error("over-long prompt must not create a builder session")
	}
}

// TestAIPromptClamp_Refine asserts the same clamp on the refine endpoint.
func TestAIPromptClamp_Refine(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "Welcome", "welcome", []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	tc.injectStreamingBuilder("run-clamp2", nil, false, nil)
	cookie, csrf := tc.login(t)

	long := strings.Repeat("b", aiPromptMaxLen+1)
	rr := tc.postForm(t, "/console/api/workflows/welcome/ai-refine", url.Values{
		"prompt":     {long},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "too long") {
		t.Errorf("expected prompt-too-long message on refine; body: %s", rr.Body.String())
	}
	if len(builderSessionIDs(t, tc)) != 0 {
		t.Error("over-long refine prompt must not create a builder session")
	}
}

// ─── 3. Streaming robustness: completed-but-missed-terminal-event ────────────

// TestAIGenerate_CompletedRunResolvesToDraft proves that when the ctx deadline
// fires but the run actually completed (session is Ready), the handler serves
// the ready draft instead of the "did not finish" error. This is the
// dropped-terminal-event case: the done event arrived after the SSE subscriber
// unsubscribed or was never received.
func TestAIGenerate_CompletedRunResolvesToDraft(t *testing.T) {
	tc := newTestConsole(t)
	draft := &aibuilder.WorkflowDraft{
		Name:              "Late Finish",
		TriggerIdentifier: "late-finish",
		Steps: []domain.Step{{
			StepID:        "s1",
			Type:          "email",
			Name:          "Greet",
			ControlValues: json.RawMessage(`{"subject":"Hi"}`),
		}},
	}
	// The stub Dispatch writes the final draft + sets status=Ready immediately
	// (simulating the run finishing). The stream emits only a reasoning event
	// (no terminal done event), so the timeout fires. On ctx-done the handler
	// must detect the Ready session and serve the preview.
	events := [][]byte{
		aiStreamMsg(t, "reasoning", "Thinking...", nil, ""),
		// intentionally no done event
	}
	tc.injectStreamingBuilder("run-late", draft, false, events)
	tc.c.aiGenerateTimeoutOverride = 30 * time.Millisecond
	tc.c.aiBuildPollOverride = 10 * time.Second // keep poll from interfering
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"late finishing workflow"},
		"csrf_token": {csrf},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Must NOT show the "did not finish" error.
	if strings.Contains(strings.ToLower(body), "did not finish") {
		t.Errorf("completed run showed 'did not finish' instead of ready draft; body: %s", body)
	}
	// Must render the Use-this-workflow preview with the ready draft.
	if !strings.Contains(body, "Use this workflow") {
		t.Errorf("completed run did not render the ready draft preview; body: %s", body)
	}
	if !strings.Contains(body, "late-finish") {
		t.Errorf("preview missing trigger identifier from ready draft; body: %s", body)
	}
}

// TestAIRateLimit_SweepsExpiredWhenLarge proves the window map evicts
// fully-expired entries once it grows past the sweep threshold, so long-idle
// scopes don't accumulate forever.
func TestAIRateLimit_SweepsExpiredWhenLarge(t *testing.T) {
	orig := aiRateLimitSweepThreshold
	aiRateLimitSweepThreshold = 3
	defer func() { aiRateLimitSweepThreshold = orig }()

	var clk time.Time
	l := &aiRateLimiter{now: func() time.Time { return clk }}

	// Fill the map with idle scopes at t=0 (reaches the threshold).
	clk = time.Unix(0, 0)
	for _, id := range []string{"a", "b", "c"} {
		l.Allow(domain.Scope{OrgID: id, EnvID: "e"})
	}
	if got := len(l.windows); got != 3 {
		t.Fatalf("pre-sweep window count = %d, want 3", got)
	}

	// Advance past the window so those entries are expired, then a fresh scope
	// opens a new window and triggers the opportunistic sweep.
	clk = clk.Add(aiRateLimitWindow + time.Millisecond)
	l.Allow(domain.Scope{OrgID: "d", EnvID: "e"})

	// a/b/c were expired and swept; only the new scope d remains.
	if _, ok := l.windows["a:e"]; ok {
		t.Error("expired scope a:e was not evicted")
	}
	if got := len(l.windows); got != 1 {
		t.Fatalf("post-sweep window count = %d, want 1 (only the fresh scope)", got)
	}
}
