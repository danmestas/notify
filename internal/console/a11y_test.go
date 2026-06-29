package console

import (
	"net/http"
	"strings"
	"testing"
)

// a11y_test.go locks the accessibility sweep (Norman interaction heuristics +
// Friedman accessible-form patterns) applied across the console: shared modal
// a11y on the .console-slideover drawers, real labels on search/filter inputs,
// the ui-select listbox/option ARIA + keyboard wiring, destructive-action
// styling, and the login secret-field show/hide toggle. The assertions are
// literal-key so a regression in the rendered wire markup fails the build.

// drawerA11yCase is one create/edit slide-over we assert the shared modal
// pattern on: aria-modal="true" plus a window-level Escape handler.
type drawerA11yCase struct {
	name        string
	path        string
	wantStrings []string
}

func TestA11y_Slideovers_ModalAndEscape(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	cases := []drawerA11yCase{
		{
			name: "workflows create",
			path: "/console/workflows/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($workflowNewOpen = false)"`,
			},
		},
		{
			name: "topics create",
			path: "/console/topics/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($topicNewOpen = false)"`,
			},
		},
		{
			name: "agents create",
			path: "/console/agents/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($agentNewOpen = false)"`,
			},
		},
		{
			name: "contexts create",
			path: "/console/contexts/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($contextNewOpen = false)"`,
			},
		},
		{
			name: "domains create",
			path: "/console/domains/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($domainNewOpen = false)"`,
			},
		},
		{
			name: "variables create",
			path: "/console/variables/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($variableNewOpen = false)"`,
			},
		},
		{
			name: "environments create",
			path: "/console/settings/environments/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($envNewOpen = false)"`,
			},
		},
		{
			name: "webhooks create",
			path: "/console/webhooks/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($webhookNewOpen = false)"`,
			},
		},
		{
			name: "layouts create (route-driven)",
			path: "/console/layouts/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`evt.key === 'Escape' && (window.location = '/console/layouts')`,
			},
		},
		{
			name: "translations create (route-driven)",
			path: "/console/translations/new",
			wantStrings: []string{
				`aria-modal="true"`,
				`evt.key === 'Escape' && (window.location = '/console/translations')`,
			},
		},
		{
			name: "integrations connect drawer",
			path: "/console/integrations",
			wantStrings: []string{
				`aria-modal="true"`,
				`data-on:keydown__window="evt.key === 'Escape' && ($integrationProviderFormOpen = false)"`,
			},
		},
	}

	for _, tcase := range cases {
		t.Run(tcase.name, func(t *testing.T) {
			rr := tc.get(t, tcase.path, cookie)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s: status=%d body=%s", tcase.path, rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			for _, want := range tcase.wantStrings {
				if !strings.Contains(body, want) {
					t.Errorf("GET %s: missing %q", tcase.path, want)
				}
			}
		})
	}
}

// TestA11y_SearchInputs_HaveAccessibleNames asserts every filter/search box
// carries a real accessible name (aria-label), so a screen reader announces
// what the field filters — not just an unnamed text box.
func TestA11y_SearchInputs_HaveAccessibleNames(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	cases := []struct {
		path  string
		label string
	}{
		{"/console/workflows", `aria-label="Search workflows"`},
		{"/console/topics", `aria-label="Search topics by name"`},
		{"/console/topics", `aria-label="Search topics by key"`},
		{"/console/domains", `aria-label="Search domains"`},
		{"/console/agents", `aria-label="Search agents by identifier or name"`},
		{"/console/variables", `aria-label="Filter variables by key"`},
		{"/console/contexts", `aria-label="Filter contexts by type"`},
		{"/console/subscribers", `aria-label="Search subscribers by name"`},
		{"/console/subscribers", `aria-label="Search subscribers by email"`},
	}
	for _, c := range cases {
		rr := tc.get(t, c.path, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d", c.path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), c.label) {
			t.Errorf("GET %s: missing search label %q", c.path, c.label)
		}
	}
}

// TestA11y_UISelect_ListboxAndOptionARIA renders a page carrying a ui-select
// and asserts the listbox has an accessible name, the trigger references the
// popover + carries keyboard wiring, and options are keyboard-reachable
// (tabindex + keydown handlers).
func TestA11y_UISelect_ListboxAndOptionARIA(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	// The workflows page renders the status ui-select ("All statuses").
	rr := tc.get(t, "/console/workflows", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/workflows: status=%d", rr.Code)
	}
	body := rr.Body.String()

	wants := []string{
		// Listbox accessible name (mirrors the placeholder).
		`role="listbox"`,
		`aria-label="All statuses"`,
		// Trigger wiring: controls the popover + reactive expanded state.
		`aria-controls="wf-status-popover"`,
		`data-attr:aria-expanded=`,
		// Options are keyboard reachable (roving tabindex) + selectable.
		`role="option" tabindex="0"`,
		`data-on:keydown=`,
		// aria-expanded and aria-selected must be ternaries yielding the literal
		// strings "true"/"false" — Datastar treats bare boolean signals as
		// presence attributes (empty-string for true, absent for false) so screen
		// readers always read "off". ARIA requires the literal strings.
		`? 'true' : 'false'`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("ui-select: missing %q", w)
		}
	}
}

// TestA11y_UISelect_TriggerKeydownExpr proves the trigger keydown opens the
// popover on ArrowDown/Enter/Space (Friedman keyboard-operable combobox) —
// asserted at the Go helper level so the expression shape is locked.
func TestA11y_UISelect_TriggerKeydownExpr(t *testing.T) {
	sel := newUISelect("st", "", "sig",
		uiSelectOptions("", "All", "a", "Active"), "All", "", false)

	// Trigger: must open on ArrowDown / Enter / Space and close on Escape.
	got := string(sel.TriggerKeydownExpr)
	for _, want := range []string{"ArrowDown", "Enter", " ", "$uiSelOpen_st = true", "Escape"} {
		if !strings.Contains(got, want) {
			t.Errorf("TriggerKeydownExpr = %q, missing %q", got, want)
		}
	}
	// Trigger must focus the first visible option on open (JS property check,
	// not fragile CSS attribute-substring selector).
	if strings.Contains(got, `style*=`) {
		t.Errorf("TriggerKeydownExpr uses fragile attribute-substring selector; want JS property check: %q", got)
	}
	if !strings.Contains(got, `.style.display`) {
		t.Errorf("TriggerKeydownExpr missing JS .style.display property check for visibility: %q", got)
	}

	if sel.ListboxLabel != "All" {
		t.Errorf("ListboxLabel = %q, want %q", sel.ListboxLabel, "All")
	}

	// Option KeydownExpr must handle all four keyboard interactions.
	if len(sel.Rows) == 0 {
		t.Fatal("no rows in ui-select")
	}
	optExpr := string(sel.Rows[0].KeydownExpr)
	for _, want := range []string{"ArrowDown", "ArrowUp", "Enter", "Escape"} {
		if !strings.Contains(optExpr, want) {
			t.Errorf("option KeydownExpr missing %q: %q", want, optExpr)
		}
	}

	// ExpandedExpr must yield the literal strings "true"/"false" for
	// aria-expanded — not a bare boolean signal. Datastar treats bare booleans
	// as presence attributes (empty-string for true, absent for false), so
	// screen readers always read the state as "off". ARIA requires the literal
	// string values.
	if !strings.Contains(string(sel.ExpandedExpr), "? 'true' : 'false'") {
		t.Errorf("ExpandedExpr: want ternary yielding string, got %q", sel.ExpandedExpr)
	}
	// SelectedARIAExpr (used for data-attr:aria-selected) must also be a
	// string ternary. SelectedExpr (used for data-show) stays boolean.
	if !strings.Contains(string(sel.Rows[0].SelectedARIAExpr), "? 'true' : 'false'") {
		t.Errorf("Rows[0].SelectedARIAExpr: want ternary yielding string, got %q", sel.Rows[0].SelectedARIAExpr)
	}
}

// TestA11y_DestructiveButtons_UseDangerTreatment asserts the delete triggers
// carry the .btn-danger danger treatment so they read as destructive and are
// visually distinct from safe actions (Norman: error prevention). It seeds a
// variable + context so the tbody Delete buttons render, and checks the
// detail-page danger-zone delete too.
func TestA11y_DestructiveButtons_UseDangerTreatment(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)

	if _, err := tc.deps.EnvVars.Set(testBackground(), tc.scope, "MINE", true, "mine-val"); err != nil {
		t.Fatalf("seed variable: %v", err)
	}
	if _, err := tc.deps.Contexts.Create(testBackground(), tc.scope, "tenant", "mine", nil); err != nil {
		t.Fatalf("seed context: %v", err)
	}

	// tbody Delete trigger (first-click) must carry btn-danger.
	for _, p := range []string{
		"/console/variables",
		"/console/contexts",
	} {
		rr := tc.get(t, p, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d", p, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "btn-outline btn-sm btn-danger") {
			t.Errorf("GET %s: tbody Delete missing btn-danger treatment", p)
		}
	}

	// Detail-page danger-zone delete must also read as destructive.
	for _, p := range []string{
		"/console/variables/MINE?confirmDelete=true",
		"/console/contexts/tenant/mine?confirmDelete=true",
	} {
		rr := tc.get(t, p, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d", p, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "btn-danger") {
			t.Errorf("GET %s: danger-zone delete missing btn-danger", p)
		}
	}
}

// TestA11y_Login_SecretToggle asserts the login API-key field gains a real
// show/hide toggle button reflecting state via aria-pressed (Friedman: never
// signal toggle state by colour alone).
func TestA11y_Login_SecretToggle(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/login", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /console/login: status=%d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="api_key_toggle"`,
		`aria-pressed="false"`,
		`aria-controls="api_key"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login secret toggle: missing %q", want)
		}
	}
}

// TestA11y_ProviderForm_NativeButtonSemantics asserts the connect-drawer
// provider rows keep native <button> semantics (no role="listitem" clobber).
// The step-1 catalog renders from the provider-form fragment endpoint.
func TestA11y_ProviderForm_NativeButtonSemantics(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/integration-provider-form", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET provider-form fragment: status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `class="integration-provider-row"`) {
		t.Fatalf("provider catalog not rendered; body=%s", body)
	}
	// The provider rows are <button>s; role="listitem" on them clobbers the
	// native button role and must be gone.
	if strings.Contains(body, `role="listitem"`) {
		t.Errorf("provider-row button still carries role=\"listitem\"")
	}
	// The group keeps a real accessible name without the list role.
	if !strings.Contains(body, `aria-label="Available providers"`) {
		t.Errorf("provider list missing group accessible name")
	}
}
