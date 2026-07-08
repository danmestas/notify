package console

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
)

// TestAICopilotLayout_ViewportClampedForInternalScroll proves the docked
// panel's outer row is height-CLAMPED (not just floored) to the viewport, so
// #ai-thread is the only element that scrolls and the composer never gets
// pushed past the fold. Regression test for the live-verified bug: the row
// only carried min-height, so a tall thread grew the whole row past the
// viewport with no internal scrollbar — clicks on the Keep/Discard/Try-again
// row (and sometimes Send) landed below document.elementFromPoint and were
// silently swallowed.
func TestAICopilotLayout_ViewportClampedForInternalScroll(t *testing.T) {
	css, err := assetsFS.ReadFile("assets/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	body := string(css)

	block := cssRuleBody(t, body, ".ai-copilot-layout")
	if !strings.Contains(block, "height:") {
		t.Errorf(".ai-copilot-layout has no height rule — the row can still grow past the viewport instead of clamping; block: %s", block)
	}
	if !strings.Contains(block, "overflow: hidden") {
		t.Errorf(".ai-copilot-layout is not overflow:hidden — a tall child can still force the row (and the page) to scroll instead of #ai-thread; block: %s", block)
	}

	thread := cssRuleBody(t, body, ".ai-thread")
	if !strings.Contains(thread, "overflow-y: auto") {
		t.Errorf(".ai-thread must be the internal scroll region (overflow-y: auto); block: %s", thread)
	}

	composer := cssRuleBody(t, body, ".ai-copilot-composer")
	if !strings.Contains(composer, "flex: none") {
		t.Errorf(".ai-copilot-composer must stay pinned (flex: none), not participate in the scroll growth; block: %s", composer)
	}
}

// cssRuleBody extracts the declaration body of the first "selector { ... }"
// rule in css, or fails the test if the selector is not found.
func cssRuleBody(t *testing.T, css, selector string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("selector %q not found in app.css", selector)
	}
	return m[1]
}

// TestAIBuilderCopilot_AutoScrollsThreadOnAppendAndFinalize proves the SSE
// relay scrolls #ai-thread to the bottom twice per turn: once right after the
// user bubble + assistant shell append (so a long prior thread doesn't hide
// the turn that was just sent), and once when the turn finishes (so the
// Keep/Discard/Try-again row that just got patched in is actually in view,
// not below the fold where a click would hit nothing).
func TestAIBuilderCopilot_AutoScrollsThreadOnAppendAndFinalize(t *testing.T) {
	tc := newTestConsole(t)
	draft := &aibuilder.WorkflowDraft{
		Name:              "Welcome Flow",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{
			{StepID: "s1", Type: "email", Name: "Greeting", ControlValues: json.RawMessage(`{"subject":"Hi"}`)},
		},
	}
	events := [][]byte{
		aiStreamMsg(t, "done", "Built a one-step welcome flow.", draft, ""),
	}
	tc.injectStreamingBuilder("run-scroll", draft, false, events)
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"welcome new users with an email"},
		"view":       {"copilot"},
		"csrf_token": {csrf},
	}, cookie)
	body := rr.Body.String()

	scrollScript := "ai-thread"
	if got := strings.Count(body, scrollScript); got < 3 {
		// #ai-thread itself is referenced by the append-mode patch selector plus
		// twice more by the autoscroll script (post-append, post-finalize).
		t.Errorf("expected #ai-thread referenced by the append patch AND two autoscroll scripts, got %d mentions; body: %s", got, body)
	}
	if got := strings.Count(body, "scrollHeight"); got < 2 {
		t.Errorf("expected 2 autoscroll scripts (post-append, post-finalize), got %d; body: %s", got, body)
	}
}
