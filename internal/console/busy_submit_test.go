// busy_submit_test.go guards QA finding L5 (docs/qa/console-qa-2026-07-09.md):
// primary submit buttons gave no feedback on click, so a slow or missed click
// was indistinguishable from a no-op. It pins two things: the shared
// .btn[aria-busy="true"] CSS busy cue (mirrors TestAssets_AnalyticsRunsLineCSSIsVisible),
// and the data-on:submit wiring that sets aria-busy + disables the submitter
// on a representative sample of the native POST→303 forms it was applied to.
package console

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestAssets_BusyButtonCSSIsVisible(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, assetManifest["app.css"], nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET app.css: status = %d, want 200", rr.Code)
	}
	css := rr.Body.String()

	busyRE := regexp.MustCompile(`(?s)\.btn\[aria-busy="true"\]\s*\{([^}]*)\}`)
	m := busyRE.FindStringSubmatch(css)
	if m == nil {
		t.Fatal(`.btn[aria-busy="true"] rule not found in app.css — submit buttons give no feedback while a request is in flight (QA L5)`)
	}
	if !strings.Contains(m[1], "opacity") {
		t.Errorf(`.btn[aria-busy="true"] rule does not dim the button: %q`, m[1])
	}

	spinRE := regexp.MustCompile(`(?s)\.btn\[aria-busy="true"\]::after\s*\{([^}]*)\}`)
	sm := spinRE.FindStringSubmatch(css)
	if sm == nil {
		t.Fatal(`.btn[aria-busy="true"]::after spinner rule not found in app.css`)
	}
	if !strings.Contains(sm[1], "animation") {
		t.Errorf(`.btn[aria-busy="true"]::after rule does not animate a spinner: %q`, sm[1])
	}
}

// formSlice extracts the HTML from a form's unique action-attribute anchor
// through its closing </form>, so assertions can be scoped to a single form
// on a page that renders several.
func formSlice(t *testing.T, body, actionAnchor string) string {
	t.Helper()
	idx := strings.Index(body, actionAnchor)
	if idx < 0 {
		t.Fatalf("form with %s not found in body:\n%s", actionAnchor, body)
	}
	end := strings.Index(body[idx:], "</form>")
	if end < 0 {
		t.Fatalf("no closing </form> found after %s", actionAnchor)
	}
	return body[idx : idx+end]
}

// assertBusyOnSubmit checks that a form carries the shared busy-on-submit
// convention: a data-on:submit handler that (only when evt.submitter is set)
// marks the submitter aria-busy and disables it.
func assertBusyOnSubmit(t *testing.T, form, label string) {
	t.Helper()
	submitRE := regexp.MustCompile(`data-on:submit="([^"]*)"`)
	m := submitRE.FindStringSubmatch(form)
	if m == nil {
		t.Fatalf("%s form missing data-on:submit busy wiring: %s", label, form)
	}
	handler := m[1]
	if !strings.Contains(handler, "evt.submitter") {
		t.Errorf("%s form's data-on:submit does not gate on evt.submitter: %q", label, handler)
	}
	if !strings.Contains(handler, "aria-busy") {
		t.Errorf("%s form's data-on:submit does not set aria-busy: %q", label, handler)
	}
	if !strings.Contains(handler, "disabled") {
		t.Errorf("%s form's data-on:submit does not disable the submitter: %q", label, handler)
	}
}

func TestWorkflowDetail_SaveFormHasBusyOnSubmitWiring(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "MyFlow", "my-flow")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-flow", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows/my-flow: status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/api/workflows/my-flow/save"`)
	assertBusyOnSubmit(t, form, "workflow save")
}

func TestSubscribers_CreateFormHasBusyOnSubmitWiring(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers: status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/subscribers"`)
	assertBusyOnSubmit(t, form, "subscriber create")
}

func TestFragment_IntegrationProviderForm_ConnectFormHasBusyOnSubmitWiring(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
		url.Values{"provider": {"smtp"}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET integration-provider-form (smtp): status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/integrations"`)
	assertBusyOnSubmit(t, form, "integration connect")
}
