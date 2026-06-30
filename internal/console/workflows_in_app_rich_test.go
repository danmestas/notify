package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestFragmentWorkflowStepEditor_InAppRichFields proves the in_app step editor
// renders form inputs for every rich field and pre-fills them from controlValues.
func TestFragmentWorkflowStepEditor_InAppRichFields(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(
			`{"subject":"S","body":"B","avatar":"https://x/a.png",` +
				`"primaryAction":{"label":"Open","redirect":{"url":"/foo","target":"_self"}},` +
				`"data":{"orderId":"123"}}`,
		)},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET in_app step-editor fragment: status = %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Form inputs for the new fields must be present (not the generic JSON editor).
	for _, name := range []string{
		`name="avatar"`,
		`name="primary_action_label"`,
		`name="primary_action_redirect_url"`,
		`name="secondary_action_label"`,
		`name="secondary_action_redirect_url"`,
		`name="redirect_url"`,
		`name="data_json"`,
	} {
		if !strings.Contains(body, name) {
			t.Errorf("in_app step-editor fragment missing input %s", name)
		}
	}
	if strings.Contains(body, `name="control_values_json"`) {
		t.Errorf("in_app step-editor fell through to generic JSON editor")
	}
	// Pre-filled values from controlValues.
	if !strings.Contains(body, `value="https://x/a.png"`) {
		t.Errorf("avatar not pre-filled; body=%s", body)
	}
	if !strings.Contains(body, `value="Open"`) {
		t.Errorf("primaryAction.label not pre-filled")
	}
	if !strings.Contains(body, `value="/foo"`) {
		t.Errorf("primaryAction.redirect.url not pre-filled")
	}
	if !strings.Contains(body, "orderId") {
		t.Errorf("data textarea not pre-filled with the data object")
	}
}

// TestWorkflowStepSave_InAppRichRoundTrip proves a save of the in_app rich form
// persists the fields into controlValues and the editor repopulates them on reload.
func TestWorkflowStepSave_InAppRichRoundTrip(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedWorkflowWithSteps(t, "My WF", "my-wf", []domain.Step{
		{StepID: "in_app-1", Type: "in_app", ControlValues: json.RawMessage(`{}`)},
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/my-wf/steps/in_app-1/save", url.Values{
		"csrf_token":                  {csrf},
		"subject":                     {"Welcome"},
		"body":                        {"Hi {{payload.name}}"},
		"avatar":                      {"https://x/a.png"},
		"primary_action_label":        {"Open order"},
		"primary_action_redirect_url": {"/orders/1"},
		"data_json":                   {`{"orderId":"7"}`},
	}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("in_app step save: status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}

	rr2 := tc.get(t, "/console/api/fragments/workflow-step-editor/my-wf/in_app-1", cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, `value="https://x/a.png"`) {
		t.Errorf("after save, avatar did not repopulate; body=%s", body)
	}
	if !strings.Contains(body, `value="Open order"`) {
		t.Errorf("after save, primaryAction.label did not repopulate")
	}
	if !strings.Contains(body, `value="/orders/1"`) {
		t.Errorf("after save, primaryAction.redirect.url did not repopulate")
	}
	if !strings.Contains(body, "orderId") {
		t.Errorf("after save, data textarea did not repopulate")
	}
}
