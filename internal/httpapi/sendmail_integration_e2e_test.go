package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPostEmailIntegration_Sendmail: the pipe provider is created from a
// sender alone, and the response shows that sender in the clear -- there is
// no secret in this integration to mask.
func TestPostEmailIntegration_Sendmail(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"local-mta","provider":"sendmail","from":"quarry <forge@example.org>","primary":true}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/email (sendmail): status=%d body=%s", status, respBody)
	}
	var created struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerId"`
		Channel    string `json:"channel"`
	}
	if err := json.Unmarshal([]byte(respBody), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ProviderID != "sendmail" || created.Channel != "email" {
		t.Errorf("providerId=%q channel=%q", created.ProviderID, created.Channel)
	}

	// The create response is the slim DTO; masked credentials come back on
	// GET, where "from" must be in the clear because nothing here is secret.
	status, getBody := doGet(t, srv.URL+"/v1/integrations/"+created.ID, authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET integration: status=%d body=%s", status, getBody)
	}
	var got struct {
		CredentialsMasked map[string]any `json:"credentialsMasked"`
	}
	if err := json.Unmarshal([]byte(getBody), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if got.CredentialsMasked["from"] != "quarry <forge@example.org>" {
		t.Errorf("from = %v, want shown in the clear (it is not a secret); body=%s", got.CredentialsMasked["from"], getBody)
	}
}

// TestPostEmailIntegration_SendmailIgnoresCommand is the API-side half of the
// security property: a client that sends a "command" field gets a sendmail
// integration with only its sender stored. The field is not rejected, because
// unknown JSON fields are ignored everywhere else in this API and rejecting
// this one alone would only document the attack; it is simply never read.
func TestPostEmailIntegration_SendmailIgnoresCommand(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"planted","provider":"sendmail","from":"f@e.org","command":"/bin/sh -c id"}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, respBody)
	}
	assertNoStoredCommand(t, srv.URL, authHeader, respBody, "/bin/sh")
}

// assertNoStoredCommand reads the integration back. The create response is the
// slim DTO and never carries credentials, so only the GET -- whose masked map
// shows every non-secret stored field -- can prove a command was not kept.
func assertNoStoredCommand(t *testing.T, base, authHeader, createBody, marker string) {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(createBody), &created); err != nil || created.ID == "" {
		t.Fatalf("decode create response: %v (%s)", err, createBody)
	}
	status, getBody := doGet(t, base+"/v1/integrations/"+created.ID, authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET integration: status=%d body=%s", status, getBody)
	}
	var got struct {
		CredentialsMasked map[string]any `json:"credentialsMasked"`
	}
	if err := json.Unmarshal([]byte(getBody), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if _, has := got.CredentialsMasked["command"]; has || strings.Contains(getBody, marker) {
		t.Fatalf("a client-supplied command was stored on the integration: %s", getBody)
	}
	if got.CredentialsMasked["from"] == nil {
		t.Fatalf("from was not stored: %s", getBody)
	}
}

// TestPostEmailIntegration_UnknownProviderListsSendmail keeps the allowed-list
// error honest now that there is one more provider.
func TestPostEmailIntegration_UnknownProviderListsSendmail(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader,
		`{"identifier":"x","provider":"postfix","from":"f@e.org"}`)
	if status != http.StatusBadRequest || !strings.Contains(respBody, "sendmail") {
		t.Fatalf("status=%d body=%s, want 400 listing sendmail", status, respBody)
	}
}

// TestPostIntegration_UnifiedCreate_Sendmail covers the unified
// /v1/integrations route, which takes credentials as a map.
func TestPostIntegration_UnifiedCreate_Sendmail(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)
	body := `{"providerId":"sendmail","identifier":"mta-unified","primary":false,"credentials":{"from":"forge@example.org","command":"/bin/evil"}}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations (sendmail): status=%d body=%s", status, respBody)
	}
	if !strings.Contains(respBody, `"providerId":"sendmail"`) {
		t.Fatalf("unexpected response: %s", respBody)
	}
	assertNoStoredCommand(t, srv.URL, authHeader, respBody, "/bin/evil")
}
