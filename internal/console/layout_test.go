// layout_test.go pins the shared partial contracts later slices build
// on: the empty-state body, the ReadOnly-aware action button, the page
// header tile validation, and the runURL deep-link helper.
package console

import (
	"strings"
	"testing"
)

func TestPartial_EmptyStateRendersHierarchy(t *testing.T) {
	tc := newTestConsole(t)
	html, err := renderFragment(tc.c.ts.base, "empty-state", emptyState{
		Icon:        "◌",
		Title:       "No workflows yet",
		Description: "Create a workflow to start delivering notifications.",
		PrimaryAction: &emptyStateAction{
			Label: "Create workflow",
			Href:  "/console/workflows/new",
		},
	})
	if err != nil {
		t.Fatalf("render empty-state: %v", err)
	}
	for _, want := range []string{
		"No workflows yet",
		"Create a workflow to start delivering notifications.",
		`href="/console/workflows/new"`,
		"Create workflow",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("empty-state missing %q", want)
		}
	}
	if strings.Contains(html, `aria-disabled="true"`) {
		t.Errorf("writable empty-state rendered a disabled action")
	}
}

func TestPartial_EmptyStateReadOnlyDisablesAction(t *testing.T) {
	tc := newTestConsole(t)
	html, err := renderFragment(tc.c.ts.base, "empty-state", emptyState{
		Title:       "No workflows yet",
		Description: "Create a workflow to start delivering notifications.",
		PrimaryAction: &emptyStateAction{
			Label: "Create workflow",
			Href:  "/console/workflows/new",
		},
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("render empty-state: %v", err)
	}
	if !strings.Contains(html, `aria-disabled="true"`) {
		t.Errorf("read-only empty-state action not disabled")
	}
	if strings.Contains(html, `href="/console/workflows/new"`) {
		t.Errorf("read-only empty-state still renders a live link")
	}
}

func TestPageHeader_ValidateRejectsBadTiles(t *testing.T) {
	good := pageHeader{Title: "Dashboard", Tiles: []tile{{Label: "sent", Count: 1, Tone: toneSuccess}}}
	if err := good.validate(); err != nil {
		t.Errorf("valid header rejected: %v", err)
	}
	bad := pageHeader{Title: "Dashboard", Tiles: []tile{{Label: "sent", Count: 1, Tone: tileTone("succes")}}}
	if err := bad.validate(); err == nil {
		t.Errorf("typo'd tone accepted — the closed tone set leaks")
	}
	untitled := pageHeader{}
	if err := untitled.validate(); err == nil {
		t.Errorf("empty title accepted")
	}
}

func TestRunURL_EmptyBaseRendersNothing(t *testing.T) {
	tc := newTestConsole(t)
	if got := tc.c.runURL("run-123"); got != "" {
		t.Errorf("runURL with unset base = %q, want empty", got)
	}
	tc.c.deps.DagnatsConsoleURL = "http://127.0.0.1:8081"
	if got := tc.c.runURL("run-123"); got != "http://127.0.0.1:8081/console/runs/run-123" {
		t.Errorf("runURL = %q", got)
	}
	if got := tc.c.runURL(""); got != "" {
		t.Errorf("runURL with empty id = %q, want empty", got)
	}
}
