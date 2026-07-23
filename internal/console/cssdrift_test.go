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
// This test is that cross-reference. New templates must only use classes that
// app.css actually defines; the allowlist below pins the drift that already
// existed when the guard landed so it can be burned down without blocking work.

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

// knownCSSDrift is pre-existing, deliberately frozen debt: classes referenced by
// a template but absent from app.css as of the guard landing. It exists so the
// guard can be enforced immediately instead of waiting on a 28-template
// restyling. Entries may only be REMOVED -- adding one means new drift shipped,
// which is exactly what this test exists to prevent.
//
// Tracked for burn-down in danmestas/notify#259.
var knownCSSDrift = map[string][]string{
	"agent_conversation.html":   {"agent-compose-box", "agent-compose-form", "agent-compose-row", "agent-compose-textarea", "agent-conversation", "agent-transcript", "agent-turn", "agent-turn-body", "agent-turn-header", "agent-turn-role", "agent-turn-time", "console-pagination", "form-hint"},
	"agent_detail.html":         {"console-form", "console-form-actions", "form-hint"},
	"agents.html":               {"console-form", "form-hint"},
	"analytics.html":            {"analytics-toolbar", "analytics-workflow-filter", "analytics-workflow-filter-label"},
	"api_keys.html":             {"console-card-body", "console-card-header", "console-card-subtitle", "console-secret-reveal", "console-secret-value"},
	"context_detail.html":       {"console-card-body", "console-card-danger", "console-card-header", "console-pre", "console-textarea", "form-actions"},
	"contexts.html":             {"console-textarea", "context-create-form", "form-error"},
	"dashboard.html":            {"dashboard-chart"},
	"domains.html":              {"domain-create-form"},
	"integrations.html":         {"integration-connect-drawer", "integration-detail-drawer", "integration-detail-empty", "integration-detail-inner", "integration-layout", "integration-signpost-link"},
	"layout.html":               {"console-env-name", "console-sidebar-extlink", "console-sidebar-footer", "theme-toggle-icon"},
	"layout_editor.html":        {"console-confirm-banner", "console-form-actions", "console-help", "console-text", "console-textarea-mono", "layout-editor-form"},
	"layouts.html":              {"layout-edit-form"},
	"stream_sources.html":       {"console-secret-reveal", "console-secret-value", "form-help", "stream-source-edit-form"},
	"subscriber_detail.html":    {"console-card-subtitle", "console-form", "console-pref-channel", "console-pref-state", "console-pref-toggle", "console-pref-toggle-form", "console-prefs-channels", "console-prefs-group", "console-prefs-workflow-label", "console-prefs-workflow-row", "form-hint", "is-enabled"},
	"subscribers.html":          {"form-hint"},
	"topic_detail.html":         {"console-pagination", "subscriber-row-actions", "topic-add-member"},
	"translation_editor.html":   {"console-card-value", "console-editor-layout", "console-editor-main", "console-editor-sidebar", "console-form-actions", "console-textarea-mono", "translation-editor-form"},
	"translation_settings.html": {"console-form-actions", "console-help"},
	"translations.html":         {"translation-create-form"},
	"variable_detail.html":      {"console-card-body", "console-card-danger", "console-card-header", "form-actions", "form-error"},
	"variables.html":            {"variable-create-form"},
	"webhooks.html":             {"webhook-edit-form"},
	"workflow_activity.html":    {"activity-col-run", "wf-activity-table"},
	"workflow_detail.html":      {"form-hint", "wf-code-block", "wf-copy-btn", "wf-payload-schema", "wf-payload-schema-title", "wf-status-current", "wf-step-controls", "wf-step-controls-summary", "wf-step-header", "wf-step-id", "wf-step-item", "wf-step-list", "wf-step-name", "wf-step-type-chip"},
	"workflow_test.html":        {"console-banner--info", "console-banner--warning", "console-form-hint", "wf-test-payload-textarea", "wf-test-run-id"},
	"workflows.html":            {"wf-create-ai-link"},
}

// TestCSS_NoUndefinedClassesInTemplates fails when a console template names a
// class app.css does not define -- the defect from #117, where the domain detail
// page rendered unstyled because badge/console-dl/console-card-body existed only
// in the template.
func TestCSS_NoUndefinedClassesInTemplates(t *testing.T) {
	defined := definedClasses(t)

	templates, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no templates found -- the guard would vacuously pass")
	}

	for _, path := range templates {
		name := filepath.Base(path)
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

// TestCSS_DriftAllowlistIsCurrent keeps the allowlist honest: once a class is
// defined in app.css (or the template stops using it), its entry must go. Without
// this, the allowlist would silently re-permit drift that was already fixed.
func TestCSS_DriftAllowlistIsCurrent(t *testing.T) {
	defined := definedClasses(t)

	for name, classes := range knownCSSDrift {
		path := filepath.Join("templates", name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("allowlist names %s, which no longer exists -- drop the entry", name)
			continue
		}

		used := make(map[string]bool)
		for _, c := range classesUsedIn(t, path) {
			used[c] = true
		}

		for _, class := range classes {
			switch {
			case defined[class]:
				t.Errorf("allowlist keeps %s/%s, but app.css now defines it -- drop it from knownCSSDrift", name, class)
			case !used[class]:
				t.Errorf("allowlist keeps %s/%s, but the template no longer uses it -- drop it from knownCSSDrift", name, class)
			}
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
