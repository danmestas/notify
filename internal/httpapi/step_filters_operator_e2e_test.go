package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// A step id is a valid condition source, so an unknown "on" is accepted, while
// an unknown operator is rejected by name.
func TestWorkflow_UnknownOperatorErrorNamesIt(t *testing.T) {
	srv, _, _, apiKey := newStepFiltersHarness(t)
	body := `{"name":"Bad","triggerIdentifier":"badop","steps":[{"type":"in_app","controlValues":{"subject":"s","body":"b"},"filters":[{"combinator":"AND","conditions":[{"on":"step-1","field":"x","operator":"NOPE","value":1}]}]}]}`
	status, resp := doPost(t, srv.URL+"/v1/workflows", apiKey, body)
	if status != http.StatusBadRequest || !strings.Contains(resp, "NOPE") {
		t.Errorf("POST: %d %s, want 400 naming NOPE", status, resp)
	}
}

func TestWorkflow_StepIDIsAValidConditionSource(t *testing.T) {
	srv, _, _, apiKey := newStepFiltersHarness(t)
	body := `{"name":"Src","triggerIdentifier":"stepsrc","steps":[{"stepId":"a","type":"in_app","controlValues":{"subject":"s","body":"b"}},{"stepId":"b","type":"in_app","controlValues":{"subject":"s","body":"b"},"filters":[{"combinator":"AND","conditions":[{"on":"a","field":"x","operator":"IS_DEFINED","value":true}]}]}]}`
	if status, resp := doPost(t, srv.URL+"/v1/workflows", apiKey, body); status != http.StatusCreated {
		t.Errorf("POST: %d %s, want 201", status, resp)
	}
}
