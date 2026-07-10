// busy_submit_test.go guards the L5 regression (QA cycle 2): PR #219 added a
// data-on:submit="…aria-busy…" handler to native POST→303 forms to show a busy
// cue. But Datastar auto-calls preventDefault() on any element carrying a
// data-on:* handler, on the assumption the expression performs the request
// itself (e.g. @post(...)). The L5 handler only set the busy state and never
// submitted, so the native submission was cancelled and NOTHING replaced it —
// Create/Save silently did nothing across the whole console. The handler was
// removed; this file keeps the (harmless, reusable) .btn[aria-busy] CSS and a
// regression guard that fails if any native POST form regains a state-only
// data-on:submit handler.
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

	// The .btn[aria-busy="true"] cue is retained for future use (e.g. a form
	// that genuinely drives its own @post request and can safely set aria-busy).
	busyRE := regexp.MustCompile(`(?s)\.btn\[aria-busy="true"\]\s*\{([^}]*)\}`)
	if busyRE.FindStringSubmatch(css) == nil {
		t.Fatal(`.btn[aria-busy="true"] rule not found in app.css`)
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

// assertNativeFormSubmits guards against the L5 hazard: a native POST form
// (one that relies on the browser's own submission → 303) must NOT carry a
// data-on:submit handler, because Datastar preventDefault()s such forms and
// our handlers never issued the request themselves, so submission was
// silently cancelled. A form that legitimately drives its own @post request
// is a different shape and would not match action="…"-anchored native forms.
func assertNativeFormSubmits(t *testing.T, form, label string) {
	t.Helper()
	if strings.Contains(form, "data-on:submit") {
		t.Errorf("%s is a native POST form but carries data-on:submit — Datastar will preventDefault() it and the native submission is cancelled (L5 regression). Form:\n%s", label, form)
	}
	if strings.Contains(form, `disabled = true`) && strings.Contains(form, "submitter") {
		t.Errorf("%s disables its submitter via a data-on handler — this can drop the submitter's value or cancel submission (L5 regression). Form:\n%s", label, form)
	}
}

func TestWorkflowDetail_SaveFormSubmitsNatively(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflow(t, "MyFlow", "my-flow")
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/my-flow", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows/my-flow: status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/api/workflows/my-flow/save"`)
	assertNativeFormSubmits(t, form, "workflow save")
}

func TestSubscribers_CreateFormSubmitsNatively(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/subscribers", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/subscribers: status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/subscribers"`)
	assertNativeFormSubmits(t, form, "subscriber create")
}

func TestFragment_IntegrationProviderForm_ConnectFormSubmitsNatively(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	rr := tc.getWithQuery(t, "/console/api/fragments/integration-provider-form",
		url.Values{"provider": {"smtp"}}.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET integration-provider-form (smtp): status = %d, want 200", rr.Code)
	}
	form := formSlice(t, rr.Body.String(), `action="/console/integrations"`)
	assertNativeFormSubmits(t, form, "integration connect")
}
