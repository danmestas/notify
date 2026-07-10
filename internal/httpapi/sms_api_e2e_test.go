package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPostWorkflow_SMSStepAccepted proves the backend no longer rejects an
// "sms" step (the phantom-channel bug): a single sms-step workflow is created
// with 201, not 422.
func TestPostWorkflow_SMSStepAccepted(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"name":"SMS Flow","triggerIdentifier":"sms-flow","steps":[{"stepId":"S1","type":"sms","controlValues":{"body":"Hi {{subscriber.firstName}}"}}]}`
	status, respBody := doPost(t, srv.URL+"/v1/workflows", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/workflows with sms step: status=%d body=%s", status, respBody)
	}
}

// TestAllowedStepTypes_IncludesSMS proves the 422 body for an invalid step type
// now enumerates "sms" among the allowed values.
func TestAllowedStepTypes_IncludesSMS(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"name":"bad","triggerIdentifier":"bad-sms-wf","steps":[{"stepId":"S1","type":"carrier_pigeon","controlValues":{}}]}`
	status, respBody := doPost(t, srv.URL+"/v1/workflows", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", status, respBody)
	}
	if !strings.Contains(respBody, "sms") {
		t.Errorf("422 body missing allowed type %q; body: %s", "sms", respBody)
	}
}

// TestPostSMSIntegration_InvalidIdentifier_422 proves POST
// /v1/integrations/sms rejects an identifier with URL-unsafe characters
// (same H2-class fix as the console handler): the identifier becomes a URL
// path segment for every integration route, so an unvalidated identifier
// would be created but permanently unreachable.
func TestPostSMSIntegration_InvalidIdentifier_422(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"provider":"twilio","identifier":"foo/bar","accountSid":"AC_sid","authToken":"tok","from":"+15550001111"}`
	status, resp := doPost(t, srv.URL+"/v1/integrations/sms", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (resp: %s)", status, resp)
	}
}

// TestPostSMSIntegration_Twilio_SecretsNeverEchoed proves POST
// /v1/integrations/sms creates a twilio integration and never echoes the auth
// token, and that a subsequent GET masks the secret field.
func TestPostSMSIntegration_Twilio_SecretsNeverEchoed(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"provider":"twilio","identifier":"twilio-test","accountSid":"AC_secret_sid","authToken":"super-secret-token","from":"+15550001111","primary":true}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/sms", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/sms: status=%d body=%s", status, respBody)
	}
	if strings.Contains(respBody, "super-secret-token") {
		t.Fatalf("create response echoed the auth token: %s", respBody)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, respBody)
	}
	if got["providerId"] != "twilio" {
		t.Errorf("providerId = %v, want twilio", got["providerId"])
	}
	if got["channel"] != "sms" {
		t.Errorf("channel = %v, want sms", got["channel"])
	}

	// GET /v1/integrations must mask the auth_token field, never leak it.
	listStatus, listBody := doGet(t, srv.URL+"/v1/integrations", authHeader)
	if listStatus != http.StatusOK {
		t.Fatalf("GET /v1/integrations: status=%d body=%s", listStatus, listBody)
	}
	if strings.Contains(listBody, "super-secret-token") {
		t.Fatalf("integrations list leaked the auth token: %s", listBody)
	}
}

// TestPostSMSIntegration_SignalWire proves the signalwire branch creates an
// integration with the sms channel.
func TestPostSMSIntegration_SignalWire(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"provider":"signalwire","identifier":"sw-test","projectId":"proj_1","token":"sw-secret","spaceUrl":"example.signalwire.com","from":"+15550001111"}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/sms", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/sms (signalwire): status=%d body=%s", status, respBody)
	}
	if strings.Contains(respBody, "sw-secret") {
		t.Fatalf("create response echoed the token: %s", respBody)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, respBody)
	}
	if got["providerId"] != "signalwire" || got["channel"] != "sms" {
		t.Errorf("got providerId=%v channel=%v, want signalwire/sms", got["providerId"], got["channel"])
	}
}

// TestPostSMSIntegration_UnknownProviderFailsClosed proves an unknown SMS
// provider is rejected with 400.
func TestPostSMSIntegration_UnknownProviderFailsClosed(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"provider":"smoke-signals","identifier":"x","from":"+1"}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/sms", authHeader, body)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown sms provider: status=%d, want 400; body: %s", status, respBody)
	}
}

// TestSMSIntegrationRoute_AuthGuard proves POST /v1/integrations/sms requires
// authentication (401 without an Authorization header).
func TestSMSIntegrationRoute_AuthGuard(t *testing.T) {
	srv, _, _ := startOperatorServer(t)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/integrations/sms",
		strings.NewReader(`{"provider":"twilio","from":"+1","accountSid":"a","authToken":"b"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /v1/integrations/sms without auth: status=%d, want 401", resp.StatusCode)
	}
}
