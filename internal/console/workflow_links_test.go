package console

import (
	"net/http"
	"strings"
	"testing"
)

// The test-workflow and per-workflow activity pages were built, routed and
// working -- and nothing linked to them. The only inbound links lived inside the
// two pages themselves, so they were reachable only by an operator who already
// knew the URL.
//
// They answer the console's two most common questions ("does this work?" and
// "why didn't it arrive?"), so functionality that cannot be found is, from the
// operator's side, indistinguishable from functionality never built.
//
// These assert the links exist so the pages cannot silently become orphaned
// again -- which is how they got that way: nothing failed when the links were
// absent.

// TestWorkflowEditor_LinksToTestAndActivity covers the page an operator lands on
// straight after authoring, which is when "test this" is the next thought.
func TestWorkflowEditor_LinksToTestAndActivity(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	body := tc.get(t, "/console/workflows/order-updates", cookie).Body.String()

	for _, want := range []string{
		"/console/workflows/order-updates/test",
		"/console/workflows/order-updates/activity",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workflow editor does not link to %s", want)
		}
	}
}

// TestWorkflowsList_LinksToTestAndActivity covers asking the same questions
// without opening the editor first.
func TestWorkflowsList_LinksToTestAndActivity(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	body := tc.get(t, "/console/workflows", cookie).Body.String()

	for _, want := range []string{
		"/console/workflows/order-updates/test",
		"/console/workflows/order-updates/activity",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workflows list does not link to %s", want)
		}
	}
}

// TestWorkflowEditor_ReadOnlyKeepsActivityHidesTest pins the split. Activity is
// a read-only view and stays available; Test fires a real trigger, so it must be
// gated with the other mutations rather than shown and then failing.
func TestWorkflowEditor_ReadOnlyKeepsActivityHidesTest(t *testing.T) {
	tc := newTestConsoleReadOnly(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	body := tc.get(t, "/console/workflows/order-updates", cookie).Body.String()

	if !strings.Contains(body, "/console/workflows/order-updates/activity") {
		t.Error("read-only mode dropped the activity link, which is a read-only view")
	}
	if strings.Contains(body, "/console/workflows/order-updates/test") {
		t.Error("read-only mode offers the test link, which fires a real trigger")
	}
}

// TestWorkflowLinkTargets_Resolve guards against linking to a route that does
// not exist: the links are only useful if they land somewhere.
func TestWorkflowLinkTargets_Resolve(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedWorkflowRaw(t, "Order Updates", "order-updates")

	for _, path := range []string{
		"/console/workflows/order-updates/test",
		"/console/workflows/order-updates/activity",
	} {
		rr := tc.get(t, path, cookie)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rr.Code)
		}
	}
}
