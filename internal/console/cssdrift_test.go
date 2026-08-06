package console

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Console templates reference CSS classes by string. An *undefined* class is
// silently harmless to the browser -- no error, no warning, the element just
// renders unstyled. Headless verification does not catch it either, because the
// page still loads and the DOM still matches. The only reliable check is a
// static cross-reference of every class a template names against the stylesheet
// that is supposed to define it.
//
// This test is that cross-reference. Every class a template names must be
// defined in app.css, except for the pre-existing drift pinned in
// knownCSSDrift, which danmestas/notify#259 is burning down to empty.

// templateActions strips {{ ... }} before any class="" parsing happens.
//
// Order matters: a class attribute may legally contain quotes inside a template
// action, e.g. class="console-tab{{if eq $page.ActiveTab "overview"}} is-active{{end}}".
// Matching class="([^"]*)" first would stop at the inner quote and yield garbage
// tokens ({{if, eq, $page.ActiveTab). Removing actions first makes the remaining
// quotes unambiguous.
var templateActions = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

var (
	classAttr   = regexp.MustCompile(`class="([^"]*)"`)
	cssSelector = regexp.MustCompile(`\.(-?[A-Za-z_][A-Za-z0-9_-]*)`)
)

// dynamicMarker replaces a stripped template action. A class token containing it
// was assembled at render time (class="chip-{{.Status}}") and cannot be verified
// statically, so it is skipped rather than reported as a false positive.
const dynamicMarker = "\x00"

// knownCSSDrift pins undefined-class references that PRE-DATE the guard covering
// the directory they live in.
//
// The guard originally globbed "templates/*.html" only, so templates/partials/
// was never checked -- 124 references across 29 partials accumulated unseen
// while the top-level templates were burned down to zero. Making the walk
// recursive surfaced them all at once; pinning them here keeps the guard
// enforcing on every file from now on, rather than leaving half the markup
// unguarded to avoid an uncomfortable list.
//
// Entries may only be REMOVED. Adding one means new drift shipped, which is
// exactly what this test exists to prevent. Burn-down tracked in #259.
var knownCSSDrift = map[string][]string{
	"partials/activity_detail_panel.html":      {"activity-detail-actions", "activity-resend-btn"},
	"partials/ai_copilot_panel.html":           {"ai-copilot-note", "ai-copilot-note-error"},
	"partials/ai_message.html":                 {"ai-action-discard", "ai-action-keep", "ai-action-retry"},
	"partials/contexts_tbody.html":             {"confirm-row"},
	"partials/environments_tbody.html":         {"confirm-row", "console-label"},
	"partials/global_conversations_tbody.html": {"console-truncate"},
	"partials/integration_detail.html":         {"integration-activity-section", "integration-cred-hint", "integration-cred-label", "integration-cred-value", "integration-detail-active", "integration-detail-readonly", "integration-detail-title", "integration-provider-label", "integration-testsend-zone", "integration-verify-btn", "integration-verify-zone"},
	"partials/integrations_grid.html":          {"integration-col-actions", "integration-col-channel", "integration-col-identifier", "integration-col-primary", "integration-col-provider", "integration-col-status", "integration-provider-label"},
	"partials/layouts_tbody.html":              {"confirm-row"},
	"partials/page_header.html":                {"page-header-tile-link"},
	"partials/stream_sources_tbody.html":       {"confirm-row", "console-input-sm", "edit-row", "form-inline-label", "stream-source-limits-form"},
	"partials/subscriber_custom_fields.html":   {"subscriber-custom-fields", "wf-condition-field", "wf-condition-row", "wf-condition-value", "wf-conditions", "wf-conditions-actions", "wf-conditions-hint", "wf-conditions-legend"},
	"partials/translations_tbody.html":         {"confirm-row"},
	"partials/variables_tbody.html":            {"confirm-row"},
	"partials/webhooks_tbody.html":             {"confirm-row"},
	"partials/workflow_activity_row.html":      {"activity-run-link"},
	"partials/workflow_activity_tbody.html":    {"activity-run-link"},
	"partials/workflow_step_editor.html":       {"checkbox-pill", "checkbox-row", "wf-condition-field", "wf-condition-on", "wf-condition-operator", "wf-condition-row", "wf-condition-value", "wf-conditions", "wf-conditions-actions", "wf-conditions-hint", "wf-conditions-legend"},
}

// TestCSS_NoUndefinedClassesInTemplates fails when a console template names a
// class app.css does not define -- the defect from #117, where the domain detail
// page rendered unstyled because badge/console-dl/console-card-body existed only
// in the template.
func TestCSS_NoUndefinedClassesInTemplates(t *testing.T) {
	defined := definedClasses(t)

	// Walks templates/ RECURSIVELY. The original guard globbed "templates/*.html"
	// only, which silently excluded templates/partials/ -- where a large share of
	// the console's markup lives, including the step editor. That blind spot let
	// the guard report a clean tree while 124 undefined references sat unchecked
	// one directory down, which is worse than no guard: it converts an unknown
	// into a false assurance.
	var templates []string
	if err := filepath.WalkDir("templates", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".html") {
			templates = append(templates, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no templates found -- the guard would vacuously pass")
	}

	for _, path := range templates {
		// Keyed by path relative to templates/ so a partial and a top-level file
		// of the same base name cannot collide in the allowlist.
		name, err := filepath.Rel("templates", path)
		if err != nil {
			t.Fatalf("relativise %s: %v", path, err)
		}

		allowed := make(map[string]bool, len(knownCSSDrift[name]))
		for _, c := range knownCSSDrift[name] {
			allowed[c] = true
		}

		var undefined []string
		for _, class := range classesUsedIn(t, path) {
			if defined[class] || allowed[class] {
				continue
			}
			undefined = append(undefined, class)
		}

		if len(undefined) > 0 {
			sort.Strings(undefined)
			t.Errorf("%s references classes not defined in assets/app.css: %s\n"+
				"Undefined classes render unstyled without any error. Either use an "+
				"existing class family (chip*, console-card*, wf-meta-*) or add the "+
				"rule to app.css.", name, strings.Join(undefined, " "))
		}
	}
}

// definedClasses returns every class name app.css defines. It over-approximates
// (a ".foo" inside a comment counts), which is the safe direction: the guard
// under-reports rather than failing on a class that is genuinely styled.
func definedClasses(t *testing.T) map[string]bool {
	t.Helper()

	css, err := os.ReadFile(filepath.Join("assets", "app.css"))
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}

	out := make(map[string]bool)
	for _, m := range cssSelector.FindAllStringSubmatch(string(css), -1) {
		out[m[1]] = true
	}
	return out
}

// classesUsedIn returns the statically-determinable class names a template
// references. Render-time-assembled classes are omitted -- see dynamicMarker.
func classesUsedIn(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	stripped := templateActions.ReplaceAllString(string(raw), dynamicMarker)

	seen := make(map[string]bool)
	var out []string
	for _, m := range classAttr.FindAllStringSubmatch(stripped, -1) {
		for _, tok := range strings.Fields(m[1]) {
			if strings.Contains(tok, dynamicMarker) || seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}

// TestCSS_DriftAllowlistIsCurrent fails when an allowlist entry stops describing
// reality: the template is gone, app.css has since defined the class, or the
// template no longer references it.
//
// #259 measures progress by knownCSSDrift shrinking, which only means anything
// if every remaining entry is still real drift. A stale entry overstates the
// work left and, worse, silently re-permits its class -- someone could
// reintroduce it into that template later and the guard would wave it through.
// Entries may only be removed, so the map cannot rot in the other direction.
func TestCSS_DriftAllowlistIsCurrent(t *testing.T) {
	defined := definedClasses(t)

	for name, classes := range knownCSSDrift {
		path := filepath.Join("templates", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("allowlist pins %q, which no longer exists -- delete the entry", name)
			continue
		}

		used := make(map[string]bool)
		for _, c := range classesUsedIn(t, path) {
			used[c] = true
		}

		for _, c := range classes {
			switch {
			case defined[c]:
				t.Errorf("allowlist pins %s -> %q, but app.css now defines that class -- delete the entry", name, c)
			case !used[c]:
				t.Errorf("allowlist pins %s -> %q, but the template no longer references it -- delete the entry", name, c)
			}
		}
	}
}
