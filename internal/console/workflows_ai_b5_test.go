package console

// Rate limiting + prompt clamp coverage for the copilot generate endpoint.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
)

// ─── Rate limiting ───────────────────────────────────────────────────────────

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
	// The rejected request must not have created a session (the guard fires before
	// Create).
	sessions := builderSessionIDs(t, tc)
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
