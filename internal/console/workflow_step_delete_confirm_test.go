package console

import (
	"net/http"
	"strings"
	"testing"
)

// Removing a workflow step used to POST immediately -- one click, no confirm, no
// undo, destroying every control value on that step. It was the only destructive
// action in the console that behaved that way; everything else uses a two-step
// inline confirm, so operators are trained by the rest of the UI to expect a
// safety net that was not there.
//
// These pin the confirm, and in particular that it is scoped to ONE step.

// stepDeleteHTML renders the workflow detail page, optionally with a
// confirmDeleteStep query for the given step.
func stepDeleteHTML(t *testing.T, tc *testConsole, cookie *http.Cookie, slug, confirmStep string) string {
	t.Helper()
	url := "/console/workflows/" + slug
	if confirmStep != "" {
		url += "?confirmDeleteStep=" + confirmStep
	}
	rr := tc.get(t, url, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d", url, rr.Code)
	}
	return rr.Body.String()
}

// TestStepDelete_RequiresConfirm proves the default render offers no immediate
// delete POST -- the destructive form only appears once a confirm is requested.
func TestStepDelete_RequiresConfirm(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	body := stepDeleteHTML(t, tc, cookie, "order-updates", "")

	if strings.Contains(body, `/steps/s1/delete"`) {
		t.Error("the un-confirmed page renders a step delete form: one click still destroys a step")
	}
	if !strings.Contains(body, "confirmDeleteStep=") {
		t.Error("no confirm affordance rendered; the remove control should ask first")
	}
}

// TestStepDelete_ConfirmIsScopedToOneStep is the important one. A page-level
// confirm flag would pass a single-step test and then let a confirm raised for
// one step delete a different one.
func TestStepDelete_ConfirmIsScopedToOneStep(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Multi", "multi-step")

	// Ask to remove a step that does not exist on this workflow: no delete form
	// should appear for any other step.
	body := stepDeleteHTML(t, tc, cookie, "multi-step", "no-such-step")

	if strings.Contains(body, "/delete\"") {
		t.Error("a confirm for one step id exposed a delete form for another step")
	}
}

// TestStepDelete_ConfirmNamesTheStep keeps the confirm honest: an operator must
// be able to see WHICH step they are about to remove before agreeing.
func TestStepDelete_ConfirmNamesTheStep(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	// Discover a real step id from the rendered page rather than assuming one.
	plain := stepDeleteHTML(t, tc, cookie, "order-updates", "")
	stepID := firstConfirmStepID(plain)
	if stepID == "" {
		t.Skip("no step rendered on the seeded workflow")
	}

	body := stepDeleteHTML(t, tc, cookie, "order-updates", stepID)
	if !strings.Contains(body, stepID) {
		t.Errorf("confirm does not name step %q", stepID)
	}
	if !strings.Contains(body, "/steps/"+stepID+"/delete") {
		t.Errorf("confirm for %q renders no delete form for it", stepID)
	}
	if !strings.Contains(body, "Cancel") {
		t.Error("confirm offers no way out")
	}
}

// firstConfirmStepID pulls a step id out of a rendered confirmDeleteStep link.
func firstConfirmStepID(body string) string {
	const marker = "confirmDeleteStep="
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	end := strings.IndexAny(rest, `"&`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// TestStepDelete_ReadOnlyHidesControl keeps read-only mode coherent: it must not
// show a confirm that then fails on submit.
func TestStepDelete_ReadOnlyHidesControl(t *testing.T) {
	tc := newTestConsoleReadOnly(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	body := stepDeleteHTML(t, tc, cookie, "order-updates", "")
	if strings.Contains(body, "confirmDeleteStep=") {
		t.Error("read-only mode offers a step remove control")
	}
}
