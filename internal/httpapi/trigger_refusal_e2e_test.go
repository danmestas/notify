package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestPostTrigger_RefusalsAreNot201: a trigger the service refuses must not
// answer 201 — callers that only check the status believed the event was
// accepted while nothing ran (#310). Unknown workflow → 404; a config-only
// draft with no steps → 422. Both keep the acknowledged:false body.
func TestPostTrigger_RefusalsAreNot201(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, _, scope, st := setupWorkflowSuite(t)

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"u1"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// Seed an active zero-step draft straight through the store (Create refuses it).
	if _, err := st.Workflows().Create(ctx, scope, domain.Workflow{
		Name: "Draft", TriggerIdentifier: "draft", Active: true, Steps: json.RawMessage(`null`),
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}

	cases := []struct {
		name, trigger string
		wantStatus    int
		wantErr       string
	}{
		{"unknown workflow", "nope", http.StatusNotFound, "workflow not found"},
		{"zero-step draft", "draft", http.StatusUnprocessableEntity, "workflow has no steps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"name":"` + tc.trigger + `","to":{"subscriberId":"u1"},"payload":{}}`
			status, resp := doPost(t, srv.URL+"/v1/events/trigger", authHeader, body)
			if status != tc.wantStatus {
				t.Fatalf("status=%d want %d body=%s", status, tc.wantStatus, resp)
			}
			if !strings.Contains(resp, `"acknowledged":false`) || !strings.Contains(resp, tc.wantErr) {
				t.Fatalf("body=%s want acknowledged:false and %q", resp, tc.wantErr)
			}
		})
	}
}
