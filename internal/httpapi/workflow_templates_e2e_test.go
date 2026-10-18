package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestWorkflowTemplates_ListAndApply: the shipped templates are listed, and
// applying one creates its workflow once, answering 200 with it untouched
// afterwards; an unknown template is a 404.
func TestWorkflowTemplates_ListAndApply(t *testing.T) {
	h := newHTTPActionHarness(t)
	status, body := doGet(t, h.srv.URL+"/v1/workflow-templates", h.authHeader)
	var list []struct {
		Name              string `json:"name"`
		TriggerIdentifier string `json:"triggerIdentifier"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list) == 0 || list[0].Name != "inbound-mail" {
		t.Fatalf("list: %d %s", status, body)
	}

	apply := func() (int, string) {
		return doPost(t, h.srv.URL+"/v1/workflow-templates/inbound-mail", h.authHeader, ``)
	}
	if status, body := apply(); status != http.StatusCreated {
		t.Fatalf("first apply: %d %s", status, body)
	}
	status, body = apply()
	var wf struct {
		TriggerIdentifier string `json:"triggerIdentifier"`
		Active            bool   `json:"active"`
		Outcome           string `json:"outcome"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &wf) != nil || wf.Outcome != "unchanged" || !wf.Active || wf.TriggerIdentifier != "inbound-mail" {
		t.Fatalf("second apply: %d %s", status, body)
	}
	if status, _ := doPost(t, h.srv.URL+"/v1/workflow-templates/nope", h.authHeader, ``); status != http.StatusNotFound {
		t.Fatalf("unknown template: %d, want 404", status)
	}
}
