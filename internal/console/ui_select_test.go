package console

import (
	"bytes"
	"strings"
	"testing"
)

// renderUISelect renders the "ui-select" partial in isolation against the base
// template tree (the same tree pages clone from), so the component's markup can
// be asserted without standing up a full page handler.
func renderUISelect(t *testing.T, s uiSelect) string {
	t.Helper()
	ts, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	var buf bytes.Buffer
	if err := ts.base.ExecuteTemplate(&buf, "ui-select", s); err != nil {
		t.Fatalf("execute ui-select: %v", err)
	}
	return buf.String()
}

// TestUISelect_RendersTriggerPopoverOptions asserts the core shape: an
// input-styled trigger, the popover, and one option row per option, each
// carrying its label and value.
func TestUISelect_RendersTriggerPopoverOptions(t *testing.T) {
	s := newUISelect(
		"test-sel", "", "mySig",
		uiSelectOptions("", "All statuses", "sent", "Sent", "failed", "Failed"),
		"All statuses", "", false,
	)
	body := renderUISelect(t, s)

	for _, want := range []string{
		`class="ui-select"`,
		`id="test-sel"`,
		`class="ui-select-trigger"`,
		`aria-haspopup="listbox"`,
		`class="ui-select-popover"`,
		`role="listbox"`,
		`role="option"`,
		`data-value="sent"`,
		`>Sent<`,
		`>Failed<`,
		`All statuses`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ui-select body missing %q\n---\n%s", want, body)
		}
	}
}

// TestUISelect_SelectedCheckBindsToSignal asserts the selected-option check is
// rendered and gated on a value match against the bound signal, so the check
// shows next to the currently-selected row.
func TestUISelect_SelectedCheckBindsToSignal(t *testing.T) {
	s := newUISelect(
		"chk-sel", "", "chkSig",
		uiSelectOptions("a", "Alpha", "b", "Bravo"),
		"Pick one", "", false,
	)
	body := renderUISelect(t, s)

	if !strings.Contains(body, `class="ui-select-option-check"`) {
		t.Errorf("missing selected-option check element\n%s", body)
	}
	// The check's visibility is bound to a value match on the signal. The
	// double-quotes in the JS literal are HTML-attribute-escaped to &#34;.
	if !strings.Contains(body, `data-show="$chkSig === &#34;a&#34;"`) {
		t.Errorf("check not gated on signal value match for option a\n%s", body)
	}
}

// TestUISelect_SearchableOnlyAboveThreshold asserts the search input appears
// only when the option count exceeds the threshold, and is absent below it.
func TestUISelect_SearchableOnlyAboveThreshold(t *testing.T) {
	// Exactly threshold (6) options → NOT searchable.
	small := newUISelect("small-sel", "", "s", manyOptions(uiSelectThreshold), "P", "", false)
	if small.Searchable {
		t.Errorf("uiSelect with %d options must not be searchable", uiSelectThreshold)
	}
	smallBody := renderUISelect(t, small)
	if strings.Contains(smallBody, `class="ui-select-search"`) {
		t.Errorf("search input rendered for non-searchable select\n%s", smallBody)
	}

	// Threshold+1 options → searchable.
	big := newUISelect("big-sel", "", "b", manyOptions(uiSelectThreshold+1), "P", "", false)
	if !big.Searchable {
		t.Errorf("uiSelect with %d options must be searchable", uiSelectThreshold+1)
	}
	bigBody := renderUISelect(t, big)
	if !strings.Contains(bigBody, `class="ui-select-search"`) {
		t.Errorf("search input missing for searchable select\n%s", bigBody)
	}
	if !strings.Contains(bigBody, `class="ui-select-empty"`) {
		t.Errorf("no-results empty state missing for searchable select\n%s", bigBody)
	}
}

// TestUISelect_HiddenInputForFormName asserts a hidden input mirroring the
// signal is rendered when a form field Name is supplied, and omitted otherwise.
func TestUISelect_HiddenInputForFormName(t *testing.T) {
	withName := newUISelect("f-sel", "runtime", "rt", uiSelectOptions("mesh", "mesh"), "Pick", "", true)
	body := renderUISelect(t, withName)
	if !strings.Contains(body, `type="hidden"`) || !strings.Contains(body, `name="runtime"`) {
		t.Errorf("form select missing hidden input carrying the value\n%s", body)
	}
	if !strings.Contains(body, `data-attr:value="$rt"`) {
		t.Errorf("hidden input not bound to signal value\n%s", body)
	}

	noName := newUISelect("n-sel", "", "x", uiSelectOptions("a", "A"), "Pick", "", false)
	if strings.Contains(renderUISelect(t, noName), `type="hidden"`) {
		t.Errorf("non-form select must not render a hidden input")
	}
}

// TestUISelect_OnChangeRunsAfterSelection asserts the OnChange expression is
// appended to the option click handler so the existing @get/reload still fires.
func TestUISelect_OnChangeRunsAfterSelection(t *testing.T) {
	s := newUISelect(
		"oc-sel", "", "ocSig",
		uiSelectOptions("x", "X"),
		"Pick", "@get('/console/api/fragments/activity-list')", false,
	)
	body := renderUISelect(t, s)
	// Single-quotes in the Datastar expression are attribute-escaped to &#39;.
	if !strings.Contains(body, `@get(&#39;/console/api/fragments/activity-list&#39;)`) {
		t.Errorf("OnChange expression not wired into option click\n%s", body)
	}
}

func manyOptions(n int) []uiSelectOption {
	out := make([]uiSelectOption, 0, n)
	for i := 0; i < n; i++ {
		v := string(rune('a' + i))
		out = append(out, uiSelectOption{Value: v, Label: strings.ToUpper(v)})
	}
	return out
}
