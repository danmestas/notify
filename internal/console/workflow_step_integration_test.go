// workflow_step_integration_test.go proves the step editor's "Integration"
// select (issue #314 stage 3): it lists only the channel's ACTIVE
// integrations, saving round-trips the chosen identifier into controlValues,
// and leaving it on "Default (primary)" omits the key entirely.
package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestFragmentWorkflowStepEditor_IntegrationSelect_ListsOnlyMatchingChannel
// proves the dropdown offers "Default (primary)" plus only the ACTIVE
// integrations on the step's own channel — not other channels, not inactive
// rows.
func TestFragmentWorkflowStepEditor_IntegrationSelect_ListsOnlyMatchingChannel(t *testing.T) {
	tc := newTestConsole(t)
	seedChatIntegration(t, tc, "telegram-primary", "telegram")
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "discord",
		Channel:    "chat",
		Identifier: "discord-secondary",
		Active:     true,
	})
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "slack",
		Channel:    "chat",
		Identifier: "slack-inactive",
		Active:     false,
	})
	tc.seedIntegration(t, domain.Integration{
		ProviderID: "twilio",
		Channel:    "sms",
		Identifier: "twilio-primary",
		Active:     true,
	})
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/chat-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET chat step-editor fragment: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	if !strings.Contains(body, `name="integration"`) {
		t.Fatalf("chat step editor missing integration select:\n%s", body)
	}
	for _, want := range []string{"Default (primary)", "telegram-primary", "discord-secondary"} {
		if !strings.Contains(body, want) {
			t.Errorf("chat step editor missing option %q", want)
		}
	}
	for _, notWant := range []string{"slack-inactive", "twilio-primary"} {
		if strings.Contains(body, notWant) {
			t.Errorf("chat step editor unexpectedly lists %q (wrong channel or inactive)", notWant)
		}
	}
}

// TestWorkflowEditor_StepSave_IntegrationRoundTrips proves saving the step
// editor form with an "integration" selection persists it into controlValues,
// and that the fragment pre-fills the select from the saved value.
func TestWorkflowEditor_StepSave_IntegrationRoundTrips(t *testing.T) {
	tc := newTestConsole(t)
	seedChatIntegration(t, tc, "telegram-primary", "telegram")
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"hi"}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/chat-1/save", url.Values{
		"csrf_token":  {csrf},
		"body":        {"hi"},
		"integration": {"telegram-primary"},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(testBackground(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if !strings.Contains(string(wf.Steps), `"integration":"telegram-primary"`) {
		t.Errorf("saved steps missing integration control value: %s", wf.Steps)
	}

	// The ui-select partial renders every option's data-value regardless of
	// selection (that's how the popover lists all choices), so a bare
	// `value="telegram-primary"` substring match can never fail — it always
	// matches an option row. The actual pre-fill signal is the initial value
	// seeded into the root div's data-signals declaration
	// (cvIntegration: "telegram-primary" in newUISelectForm's signalsDecl),
	// HTML-attribute-escaped by html/template ('"' -> "&#34;").
	rr2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/chat-1", cookie)
	if !strings.Contains(rr2.Body.String(), `cvIntegration: &#34;telegram-primary&#34;`) {
		t.Errorf("step editor fragment does not pre-fill saved integration:\n%s", rr2.Body.String())
	}
}

// TestWorkflowEditor_StepSave_IntegrationDefaultOmitsKey proves saving with the
// "Default (primary)" option (empty value) omits the "integration" key from
// controlValues entirely, rather than persisting an empty string.
func TestWorkflowEditor_StepSave_IntegrationDefaultOmitsKey(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "chat-1", Type: "chat", ControlValues: json.RawMessage(`{"body":"hi","integration":"telegram-primary"}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/chat-1/save", url.Values{
		"csrf_token":  {csrf},
		"body":        {"hi"},
		"integration": {""},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	wf, err := tc.deps.Authoring.GetBySlug(testBackground(), tc.scope, "my-wf")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if strings.Contains(string(wf.Steps), `"integration"`) {
		t.Errorf("saved steps should omit integration key on default selection: %s", wf.Steps)
	}
}

// TestFragmentWorkflowStepEditor_IntegrationSelect_AbsentForNonSendSteps
// proves in_app (no integration control) never renders the select.
func TestFragmentWorkflowStepEditor_IntegrationSelect_AbsentForNonSendSteps(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"S","body":"B"}`)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET in_app step-editor fragment: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `name="integration"`) {
		t.Errorf("in_app step editor unexpectedly renders an integration select")
	}
}
