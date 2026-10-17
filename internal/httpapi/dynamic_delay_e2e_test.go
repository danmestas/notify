package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
)

// TestDynamicDelay_WaitsInTheEngine: a dynamic delay with a declared kind is
// slept by the engine itself. The harness registers no delay.dynamic handler,
// so the in_app message can only arrive if the engine timed the wait from the
// value the trigger resolved into the run input, for both kinds.
func TestDynamicDelay_WaitsInTheEngine(t *testing.T) {
	h := newHTTPActionHarness(t)
	h.createSubscriber(t, "dd_rel")
	h.createSubscriber(t, "dd_inst")

	cases := []struct {
		identifier, subscriber, kind, payload string
	}{
		{"delay-relative", "dd_rel", "relative", `{"wait":{"amount":2,"unit":"seconds"}}`},
		{"delay-instant", "dd_inst", "instant", `{"wait":"` + time.Now().Add(2*time.Second).UTC().Format(time.RFC3339) + `"}`},
	}
	for _, c := range cases {
		if _, err := h.auth.Create(h.ctx, h.scope, authoring.WorkflowInput{
			Name:              c.identifier,
			TriggerIdentifier: c.identifier,
			Steps: []domain.Step{
				{StepID: "d1", Type: "delay", ControlValues: json.RawMessage(`{"type":"dynamic","dynamicKey":"payload.wait","dynamicKind":"` + c.kind + `"}`)},
				{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"S","body":"after the wait"}`)},
			},
		}); err != nil {
			t.Fatalf("%s: create: %v", c.identifier, err)
		}
		if err := h.auth.Publish(h.ctx, h.scope, c.identifier); err != nil {
			t.Fatalf("%s: publish: %v", c.identifier, err)
		}
	}

	start := time.Now()
	for _, c := range cases {
		body := `{"name":"` + c.identifier + `","to":{"subscriberId":"` + c.subscriber + `"},"payload":` + c.payload + `,"transactionId":"tx-` + c.identifier + `"}`
		if status, rb := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader, body); status != http.StatusCreated {
			t.Fatalf("%s: trigger: %d %s", c.identifier, status, rb)
		}
	}
	for _, c := range cases {
		m := h.pollMessage(t, c.subscriber)
		if m.Status != "sent" || m.Content != "after the wait" {
			t.Fatalf("%s: message = %+v", c.identifier, m)
		}
	}
	// Both runs waited: neither message can have arrived before about two
	// seconds (the instant was rounded down to a whole second).
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("messages arrived after %v, before the delay elapsed", elapsed)
	}
}
