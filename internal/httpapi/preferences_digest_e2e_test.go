package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
)

// digestPrefsResponse decodes the preferences body INCLUDING the sibling
// digests map. It deliberately keeps Workflows as map[string]map[string]bool:
// that a consumer's existing typed decode still works is the property the
// sibling shape was chosen for (#336).
type digestPrefsResponse struct {
	Global    map[string]bool            `json:"global"`
	Workflows map[string]map[string]bool `json:"workflows"`
	Digests   map[string]struct {
		Amount int64  `json:"amount"`
		Unit   string `json:"unit"`
		Source string `json:"source"`
	} `json:"digests"`
}

// seedDigestWorkflow publishes a workflow whose first step is a regular digest.
func seedDigestWorkflow(t *testing.T, au *authoring.Service, scope domain.Scope, identifier string, controls string) {
	t.Helper()
	ctx := context.Background()
	if _, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name:              identifier,
		TriggerIdentifier: identifier,
		Steps: []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(controls)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Hi","body":"hello"}`)},
		},
	}); err != nil {
		t.Fatalf("authoring.Create(%s): %v", identifier, err)
	}
	if err := au.Publish(ctx, scope, identifier); err != nil {
		t.Fatalf("authoring.Publish(%s): %v", identifier, err)
	}
}

func getDigestPrefs(t *testing.T, url, authHeader string) digestPrefsResponse {
	t.Helper()
	status, body := doGet(t, url, authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET preferences: status=%d body=%s", status, body)
	}
	var resp digestPrefsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	return resp
}

// TestPreferences_DigestsReportEffectiveWindow covers what quarry asked for on
// the issue: read back the cadence in force, not the default you happened to
// send. A workflow with no override reports the step's window as
// source=workflow; after a PUT it reports the subscriber's as source=subscriber.
func TestPreferences_DigestsReportEffectiveWindow(t *testing.T) {
	srv, authHeader, scope, au, _, _, _ := setupPreferencesSuite(t)
	seedDigestWorkflow(t, au, scope, "quarry-activity", `{"type":"regular","amount":30,"unit":"minutes"}`)
	id := createPrefSubscriber(t, srv, authHeader, "sub-digest")
	url := srv.URL + "/v1/subscribers/" + id + "/preferences"

	resp := getDigestPrefs(t, url, authHeader)
	got, ok := resp.Digests["quarry-activity"]
	if !ok {
		t.Fatalf("digests missing the workflow; a consumer cannot show the cadence in force. got %+v", resp.Digests)
	}
	if got.Amount != 30 || got.Unit != "minutes" || got.Source != "workflow" {
		t.Fatalf("digests[quarry-activity] = %+v, want the step's 30 minutes, source=workflow", got)
	}

	status, body := doPut(t, url, authHeader, `{"digests":{"quarry-activity":{"amount":2,"unit":"hours"}}}`)
	if status != http.StatusOK {
		t.Fatalf("PUT digests: status=%d body=%s", status, body)
	}

	resp = getDigestPrefs(t, url, authHeader)
	got = resp.Digests["quarry-activity"]
	if got.Amount != 2 || got.Unit != "hours" || got.Source != "subscriber" {
		t.Fatalf("digests[quarry-activity] = %+v, want the override 2 hours, source=subscriber", got)
	}
}

// TestPreferences_DigestNullClearsOverride: PUT merges rather than replaces, so
// removing an override needs an explicit null. Omitting the key must leave it
// alone.
func TestPreferences_DigestNullClearsOverride(t *testing.T) {
	srv, authHeader, scope, au, _, _, _ := setupPreferencesSuite(t)
	seedDigestWorkflow(t, au, scope, "quarry-activity", `{"type":"regular","amount":30,"unit":"minutes"}`)
	id := createPrefSubscriber(t, srv, authHeader, "sub-clear")
	url := srv.URL + "/v1/subscribers/" + id + "/preferences"

	if status, body := doPut(t, url, authHeader, `{"digests":{"quarry-activity":{"amount":2,"unit":"hours"}}}`); status != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", status, body)
	}
	// An unrelated PUT must not disturb it.
	if status, body := doPut(t, url, authHeader, `{"global":{"email":false}}`); status != http.StatusOK {
		t.Fatalf("PUT global: status=%d body=%s", status, body)
	}
	if got := getDigestPrefs(t, url, authHeader).Digests["quarry-activity"]; got.Source != "subscriber" {
		t.Fatalf("override lost after an unrelated PUT: %+v", got)
	}

	if status, body := doPut(t, url, authHeader, `{"digests":{"quarry-activity":null}}`); status != http.StatusOK {
		t.Fatalf("PUT null: status=%d body=%s", status, body)
	}
	got := getDigestPrefs(t, url, authHeader).Digests["quarry-activity"]
	if got.Amount != 30 || got.Unit != "minutes" || got.Source != "workflow" {
		t.Fatalf("after clearing = %+v, want the step's window back with source=workflow", got)
	}
}

// TestPreferences_DigestValidation covers the refusals, including quarry's
// suggestion that a window on a workflow which never digests is worth rejecting
// rather than storing something nothing reads.
func TestPreferences_DigestValidation(t *testing.T) {
	srv, authHeader, scope, au, _, _, _ := setupPreferencesSuite(t)
	seedDigestWorkflow(t, au, scope, "quarry-activity", `{"type":"regular","amount":30,"unit":"minutes"}`)
	id := createPrefSubscriber(t, srv, authHeader, "sub-invalid")
	url := srv.URL + "/v1/subscribers/" + id + "/preferences"

	tests := []struct{ name, body string }{
		// quarry-mention is seeded by setupPreferencesSuite with an in_app step only.
		{"workflow with no digest step", `{"digests":{"quarry-mention":{"amount":2,"unit":"hours"}}}`},
		{"unknown trigger identifier", `{"digests":{"nope":{"amount":2,"unit":"hours"}}}`},
		{"zero amount", `{"digests":{"quarry-activity":{"amount":0,"unit":"hours"}}}`},
		{"negative amount", `{"digests":{"quarry-activity":{"amount":-1,"unit":"hours"}}}`},
		{"unknown unit", `{"digests":{"quarry-activity":{"amount":2,"unit":"fortnights"}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doPut(t, url, authHeader, tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body=%s", status, body)
			}
		})
	}

	// A refused PUT must not have written anything.
	if got := getDigestPrefs(t, url, authHeader).Digests["quarry-activity"]; got.Source != "workflow" {
		t.Fatalf("a rejected PUT left state behind: %+v", got)
	}
}

// TestPreferences_ScheduledDigestHasNoWindow: a cron cadence is a schedule, not
// a duration, so it is absent from digests and cannot be overridden.
func TestPreferences_ScheduledDigestHasNoWindow(t *testing.T) {
	srv, authHeader, scope, au, _, _, _ := setupPreferencesSuite(t)
	seedDigestWorkflow(t, au, scope, "quarry-daily", `{"type":"scheduled","cron":"0 9 * * *"}`)
	id := createPrefSubscriber(t, srv, authHeader, "sub-cron")
	url := srv.URL + "/v1/subscribers/" + id + "/preferences"

	if _, ok := getDigestPrefs(t, url, authHeader).Digests["quarry-daily"]; ok {
		t.Fatal("a scheduled digest reported an overridable window")
	}
	if status, _ := doPut(t, url, authHeader, `{"digests":{"quarry-daily":{"amount":2,"unit":"hours"}}}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT on a scheduled digest = %d, want 422", status)
	}
}

// TestPreferences_WorkflowsMapStaysBooleans is the compatibility property the
// sibling shape exists for: an existing client decoding workflows into
// map[string]map[string]bool keeps working once digests are present.
func TestPreferences_WorkflowsMapStaysBooleans(t *testing.T) {
	srv, authHeader, scope, au, _, _, _ := setupPreferencesSuite(t)
	seedDigestWorkflow(t, au, scope, "quarry-activity", `{"type":"regular","amount":30,"unit":"minutes"}`)
	id := createPrefSubscriber(t, srv, authHeader, "sub-compat")
	url := srv.URL + "/v1/subscribers/" + id + "/preferences"

	if status, body := doPut(t, url, authHeader,
		`{"workflows":{"quarry-activity":{"in_app":false}},"digests":{"quarry-activity":{"amount":2,"unit":"hours"}}}`); status != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", status, body)
	}

	// The pre-#336 client shape, unchanged.
	var legacy struct {
		Global    map[string]bool            `json:"global"`
		Workflows map[string]map[string]bool `json:"workflows"`
	}
	_, body := doGet(t, url, authHeader)
	if err := json.Unmarshal([]byte(body), &legacy); err != nil {
		t.Fatalf("a pre-existing client can no longer decode preferences: %v\nbody=%s", err, body)
	}
	if v, ok := legacy.Workflows["quarry-activity"]["in_app"]; !ok || v {
		t.Fatalf("workflows[quarry-activity][in_app] = %v (present=%v), want false", v, ok)
	}
}
