// browser_smoke_test.go drives a headless Chrome via the locally-
// installed `agent-browser` CLI against a live httptest.Server — the
// dagnats console smoke pattern. It pins the two things Go-side tests
// can't see: the vendored Datastar module actually bootstraps in a real
// browser, and the login → dashboard flow works with real form
// submission + cookies.
//
// Methodology (dagnats browser_smoke_test.go port):
//   - Skip cleanly when `agent-browser` is not installed or when
//     NOTIFY_SKIP_BROWSER_SMOKE=1. CI runners without Chrome stay green.
//   - Per-test agent-browser session names so parallel tests never
//     close each other's browser.
//   - Bounded everything: 30s deadline on the full test, 10s per
//     subprocess via the shared context.
//   - Minimum 2 assertions per test (positive + negative space).
package console

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

func skipIfBrowserUnavailable(t *testing.T) {
	t.Helper()
	if os.Getenv("NOTIFY_SKIP_BROWSER_SMOKE") == "1" {
		t.Skip("NOTIFY_SKIP_BROWSER_SMOKE=1; skipping browser smoke")
	}
	if _, err := exec.LookPath("agent-browser"); err != nil {
		t.Skipf("agent-browser not installed: %v", err)
	}
	// LookPath proves a FILE exists on PATH, not that it runs. Under shim-based
	// toolchains (mise/asdf) the shim is always present, so a runner whose
	// toolchain lost its language version resolves the shim happily and then
	// fails on first invocation -- turning an optional browser smoke into a red
	// suite. Probe an actual invocation so "unavailable" means what it says.
	//
	// The skip message is deliberately loud: this test has caught real bugs that
	// unit tests missed (see the login CSRF token-rotation regression), so a
	// silent skip would quietly remove coverage nobody notices is gone.
	probe := exec.Command("agent-browser", "--help")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("agent-browser present on PATH but not runnable (%v); browser smoke coverage is OFF on this machine. Output: %s",
			err, strings.TrimSpace(string(out)))
	}
}

// browserSession derives an agent-browser session name unique to the
// running test so parallel tests never share a Chrome session.
func browserSession(t *testing.T) string {
	t.Helper()
	name := "notify-" + strings.ReplaceAll(t.Name(), "/", "-")
	if name == "notify-" {
		t.Fatalf("browserSession: derived name is empty")
	}
	return name
}

// runAgentBrowser shells out to `agent-browser <args...>` pinned to the
// test's isolated session, failing the test on any non-zero exit.
func runAgentBrowser(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	if len(args) == 0 {
		t.Fatalf("runAgentBrowser: no args")
	}
	full := append([]string{args[0], "--session", browserSession(t)}, args[1:]...)
	cmd := exec.CommandContext(ctx, "agent-browser", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent-browser %s: %v\noutput: %s", strings.Join(full, " "), err, out)
	}
	return string(out)
}

// closeBrowser tears the per-test session down; logs rather than fails
// so a stuck browser doesn't poison later tests.
func closeBrowser(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "agent-browser", "close", "--session", browserSession(t))
	if err := cmd.Run(); err != nil {
		t.Logf("agent-browser close (cleanup): %v", err)
	}
}

// evalString runs `agent-browser eval` and returns the result as a
// string, tolerating bare-JSON and {value: ...} output shapes.
func evalString(t *testing.T, ctx context.Context, expr string) string {
	t.Helper()
	if expr == "" {
		t.Fatalf("evalString: empty expression")
	}
	out := strings.TrimSpace(runAgentBrowser(t, ctx, "eval", expr))
	if out == "" {
		return ""
	}
	var bare string
	if err := json.Unmarshal([]byte(out), &bare); err == nil {
		return bare
	}
	var structured struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(out), &structured); err == nil && structured.Value != "" {
		return structured.Value
	}
	return strings.Trim(out, "\"")
}

// TestBrowser_loginFlowAndDatastarBootstrap walks the real operator
// path: open the login page, paste the bootstrap key, submit, land on
// the dashboard, and verify the Datastar module bootstrapped (the Go
// tests prove the SSE wire format; only a browser proves the bundle
// runs).
func TestBrowser_loginFlowAndDatastarBootstrap(t *testing.T) {
	skipIfBrowserUnavailable(t)
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
	})
	srv := httptest.NewServer(tc.h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { closeBrowser(t) })

	runAgentBrowser(t, ctx, "open", srv.URL+"/console/login")
	time.Sleep(500 * time.Millisecond)

	// Login page rendered with the key input.
	if got := evalString(t, ctx, "document.getElementById('api_key') ? 'yes' : 'no'"); got != "yes" {
		t.Fatalf("login page missing #api_key input (got %q)", got)
	}

	// Fill + submit the real form; the browser follows the 303.
	js := "document.getElementById('api_key').value = " +
		jsString(tc.key) + ";" +
		"document.querySelector('form').submit(); 'submitted'"
	if got := evalString(t, ctx, js); got != "submitted" {
		t.Fatalf("form submit eval returned %q", got)
	}
	time.Sleep(1 * time.Second)

	if got := evalString(t, ctx, "location.pathname"); got != "/console/" {
		t.Fatalf("post-login pathname = %q, want /console/", got)
	}
	if got := evalString(t, ctx, "document.querySelector('.console-topbar-title')?.textContent || ''"); got != "Dashboard" {
		t.Errorf("dashboard title = %q, want Dashboard", got)
	}
	// Datastar bootstrap: the module namespace must land on window with
	// the plugin-registration surface callable (v1.0.x exports no
	// `apply` — the engine auto-applies on registration; `attribute` is
	// the stable export the bundle must expose).
	if got := evalString(t, ctx, "typeof window.datastar"); got != "object" {
		t.Errorf("window.datastar type = %q, want object — vendored bundle inert", got)
	}
	if got := evalString(t, ctx, "typeof window.datastar?.attribute"); got != "function" {
		t.Errorf("window.datastar.attribute type = %q, want function", got)
	}
	// Theme toggle present and wired.
	if got := evalString(t, ctx, "document.getElementById('theme-toggle') ? 'yes' : 'no'"); got != "yes" {
		t.Errorf("theme toggle missing from the header")
	}

	// PatchElements round-trip: the only thing Go-side tests can't see is
	// the engine actually wiring data-on:click and morphing the DOM from
	// the SSE event. The deliveries-by-status strip (#dashboard-kpis) is
	// the patch target; the chart's "all" channel pill is the trigger.
	// Seed one more delivery server-side, click the pill, and the
	// deliveries count must go 1 → 2 without a reload.
	before := evalString(t, ctx,
		"document.querySelector('#dashboard-kpis .page-header-tile-count').textContent")
	if before != "1" {
		t.Fatalf("pre-refresh deliveries count = %q, want 1", before)
	}
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
	})
	evalString(t, ctx,
		"document.querySelector('.dashboard-chart-toggles .pill-channel').click(); 'clicked'")
	patched := ""
	const maxPolls = 20
	for i := 0; i < maxPolls; i++ {
		time.Sleep(250 * time.Millisecond)
		patched = evalString(t, ctx,
			"document.querySelector('#dashboard-kpis .page-header-tile-count').textContent")
		if patched == "2" {
			break
		}
	}
	if patched != "2" {
		t.Errorf("deliveries count after channel-pill click = %q, want 2 — PatchElements round-trip failed", patched)
	}
	if got := evalString(t, ctx, "location.pathname"); got != "/console/" {
		t.Errorf("pill click caused a navigation to %q — patch should be in-place", got)
	}
}

// jsString quotes s as a JS string literal via JSON encoding.
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic("jsString: " + err.Error())
	}
	return string(b)
}
