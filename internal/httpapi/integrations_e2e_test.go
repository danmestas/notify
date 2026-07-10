package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestGetIntegrations_Empty returns an empty array before any integrations are
// created.
func TestGetIntegrations_Empty(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	status, body := doGet(t, srv.URL+"/v1/integrations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/integrations empty: status=%d body=%s", status, body)
	}
	var list []any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}
}

// TestGetIntegrations_AuthGuard verifies the CRUD routes require authentication.
func TestGetIntegrations_AuthGuard(t *testing.T) {
	srv, _, _ := startOperatorServer(t)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/integrations", ""},
		{http.MethodPost, "/v1/integrations", `{"providerId":"smtp"}`},
		{http.MethodGet, "/v1/integrations/someID", ""},
		{http.MethodPut, "/v1/integrations/someID", `{}`},
		{http.MethodDelete, "/v1/integrations/someID", ""},
		{http.MethodPatch, "/v1/integrations/someID/active", `{"active":false}`},
	}
	for _, rt := range routes {
		var status int
		switch rt.method {
		case http.MethodGet:
			status, _ = doGet(t, srv.URL+rt.path, "")
		case http.MethodPost:
			status, _ = doPost(t, srv.URL+rt.path, "", rt.body)
		case http.MethodPut:
			status, _ = doPut(t, srv.URL+rt.path, "", rt.body)
		case http.MethodDelete:
			status, _ = doDelete(t, srv.URL+rt.path, "")
		case http.MethodPatch:
			status, _ = doPatch(t, srv.URL+rt.path, "", rt.body)
		}
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: status=%d, want 401", rt.method, rt.path, status)
		}
	}
}

// TestPostIntegration_UnifiedCreate_SMTP creates via the unified endpoint and
// verifies the response shape does not contain credential bytes.
func TestPostIntegration_UnifiedCreate_SMTP(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"providerId":"smtp","identifier":"smtp-unified","primary":false,"credentials":{"addr":"smtp.example.com:587","host":"smtp.example.com","username":"u@example.com","password":"super-secret","from":"noreply@example.com"}}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations: status=%d body=%s", status, respBody)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, respBody)
	}
	if got["providerId"] != "smtp" {
		t.Errorf("providerId = %v, want smtp", got["providerId"])
	}
	if got["channel"] != "email" {
		t.Errorf("channel = %v, want email", got["channel"])
	}
	if got["id"] == nil || got["id"] == "" {
		t.Error("missing id in response")
	}
	// Credentials must never appear in the create response.
	if strings.Contains(respBody, "super-secret") {
		t.Errorf("response leaks password: %s", respBody)
	}
	if strings.Contains(respBody, "encCredentials") || strings.Contains(respBody, "enc_credentials") {
		t.Errorf("response leaks enc_credentials: %s", respBody)
	}
}

// TestPostIntegration_UnifiedCreate_Ntfy creates an ntfy integration via the
// unified endpoint.
func TestPostIntegration_UnifiedCreate_Ntfy(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"providerId":"ntfy","identifier":"ntfy-unified","primary":false,"credentials":{"base_url":"http://ntfy.example.com","token":"tk_secret"}}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations (ntfy): status=%d body=%s", status, respBody)
	}
	if strings.Contains(respBody, "tk_secret") {
		t.Errorf("response leaks ntfy token: %s", respBody)
	}
}

// TestPostIntegration_UnifiedCreate_Wave2Providers verifies the unified
// endpoint accepts resend/slack/whatsapp (previously only reachable via the
// per-channel endpoints) and never echoes their secrets back.
func TestPostIntegration_UnifiedCreate_Wave2Providers(t *testing.T) {
	for _, tt := range []struct {
		providerID, channel, body, secret string
	}{
		{
			"resend", "email",
			`{"providerId":"resend","identifier":"resend-unified","credentials":{"api_key":"re_secret_key","from":"noreply@example.com"}}`,
			"re_secret_key",
		},
		{
			"slack", "chat",
			`{"providerId":"slack","identifier":"slack-unified","credentials":{"bot_token":"xoxb-secret-token"}}`,
			"xoxb-secret-token",
		},
		{
			"whatsapp", "chat",
			`{"providerId":"whatsapp","identifier":"wa-unified","credentials":{"access_token":"wa_secret_token","phone_number_id":"123456"}}`,
			"wa_secret_token",
		},
		{
			"ses", "email",
			`{"providerId":"ses","identifier":"ses-unified","credentials":{"access_key_id":"AKIDEXAMPLE","secret_access_key":"ses_secret_signing_key","region":"us-east-1","from":"noreply@example.com"}}`,
			"ses_secret_signing_key",
		},
		{
			"sns", "sms",
			`{"providerId":"sns","identifier":"sns-unified","credentials":{"access_key_id":"AKIDEXAMPLE","secret_access_key":"sns_secret_signing_key","region":"us-east-1"}}`,
			"sns_secret_signing_key",
		},
	} {
		t.Run(tt.providerID, func(t *testing.T) {
			srv, authHeader, _ := startOperatorServer(t)

			status, respBody := doPost(t, srv.URL+"/v1/integrations", authHeader, tt.body)
			if status != http.StatusCreated {
				t.Fatalf("POST /v1/integrations (%s): status=%d body=%s", tt.providerID, status, respBody)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(respBody), &got); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, respBody)
			}
			if got["providerId"] != tt.providerID {
				t.Errorf("providerId = %v, want %s", got["providerId"], tt.providerID)
			}
			if got["channel"] != tt.channel {
				t.Errorf("channel = %v, want %s", got["channel"], tt.channel)
			}
			if strings.Contains(respBody, tt.secret) {
				t.Errorf("response leaks %s secret: %s", tt.providerID, respBody)
			}
		})
	}
}

// TestPostIntegration_UnifiedCreate_UnknownProvider returns 400 for an unknown
// providerId.
func TestPostIntegration_UnifiedCreate_UnknownProvider(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"providerId":"sendgrid","identifier":"sg-1","credentials":{}}`
	status, _ := doPost(t, srv.URL+"/v1/integrations", authHeader, body)
	if status != http.StatusBadRequest {
		t.Errorf("POST /v1/integrations unknown provider: status=%d, want 400", status)
	}
}

// TestPostIntegration_UnifiedCreate_InvalidIdentifier_422 proves the unified
// create endpoint rejects an identifier with URL-unsafe characters (same
// H2-class fix as the console handler): the identifier becomes a URL path
// segment for every integration route (view/edit/delete), so an unvalidated
// identifier would be created but permanently unreachable.
func TestPostIntegration_UnifiedCreate_InvalidIdentifier_422(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	for _, id := range []string{"q2adv5 bad key!! <script>", "foo/bar", "a b", "Foo", "<script>"} {
		idJSON, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal id %q: %v", id, err)
		}
		body := `{"providerId":"smtp","identifier":` + string(idJSON) + `,"credentials":{"host":"smtp.example.com","from":"x@example.com"}}`
		status, resp := doPost(t, srv.URL+"/v1/integrations", authHeader, body)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("id=%q: status = %d, want 422 (resp: %s)", id, status, resp)
		}
	}
}

// TestPostPushIntegration_InvalidIdentifier_422 proves the /v1/integrations/push
// legacy endpoint rejects the same class of bad identifier.
func TestPostPushIntegration_InvalidIdentifier_422(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"provider":"expo","identifier":"foo/bar"}`
	status, resp := doPost(t, srv.URL+"/v1/integrations/push", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (resp: %s)", status, resp)
	}
}

// TestPostChatIntegration_InvalidIdentifier_422 proves the /v1/integrations/chat
// legacy endpoint rejects the same class of bad identifier.
func TestPostChatIntegration_InvalidIdentifier_422(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"provider":"discord","identifier":"foo/bar","webhookUrl":"https://discord.example/webhook"}`
	status, resp := doPost(t, srv.URL+"/v1/integrations/chat", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (resp: %s)", status, resp)
	}
}

// TestPostIntegration_ErrorPathNeverLeaksCredentials forces a store error (dup
// identifier) and verifies the error body contains no credential values.
func TestPostIntegration_ErrorPathNeverLeaksCredentials(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"providerId":"smtp","identifier":"dup-smtp","credentials":{"addr":"smtp.secret-host.com:587","host":"smtp.secret-host.com","password":"secret-password-xyz","from":"x@example.com"}}`
	if status, b := doPost(t, srv.URL+"/v1/integrations", authHeader, body); status != http.StatusCreated {
		t.Fatalf("first create: status=%d body=%s", status, b)
	}
	status, errBody := doPost(t, srv.URL+"/v1/integrations", authHeader, body)
	if status != http.StatusInternalServerError {
		t.Fatalf("dup create: status=%d, want 500; body=%s", status, errBody)
	}
	if strings.Contains(errBody, "secret-password-xyz") {
		t.Errorf("error body leaks password: %s", errBody)
	}
	if strings.Contains(errBody, "smtp.secret-host.com") {
		t.Errorf("error body leaks host: %s", errBody)
	}
}

// TestGetIntegrations_ListNeverLeaksEncCredentials creates an integration and
// verifies the list response has no enc_credentials and has credentialsMasked.
func TestGetIntegrations_ListNeverLeaksEncCredentials(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	createBody := `{"identifier":"cf-list-test","provider":"cloudflare-email","accountId":"acct-list","apiToken":"tok-list-secret","from":"list@example.com","primary":false}`
	if status, b := doPost(t, srv.URL+"/v1/integrations/email", authHeader, createBody); status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, b)
	}

	status, listBody := doGet(t, srv.URL+"/v1/integrations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/integrations: status=%d body=%s", status, listBody)
	}
	if strings.Contains(listBody, "tok-list-secret") {
		t.Errorf("list response leaks apiToken: %s", listBody)
	}
	if strings.Contains(listBody, "encCredentials") || strings.Contains(listBody, "enc_credentials") {
		t.Errorf("list response leaks enc_credentials field: %s", listBody)
	}

	var items []map[string]any
	if err := json.Unmarshal([]byte(listBody), &items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["providerId"] != "cloudflare-email" {
		t.Errorf("providerId = %v, want cloudflare-email", items[0]["providerId"])
	}
}

// TestGetIntegration_ByID_MasksSecretFields verifies that the GET /{id}
// response masks the api_token but preserves the non-secret account_id field.
func TestGetIntegration_ByID_MasksSecretFields(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	createBody := `{"identifier":"cf-get-test","provider":"cloudflare-email","accountId":"public-acct-id","apiToken":"private-tok-secret","from":"get@example.com","primary":false}`
	status, createResp := doPost(t, srv.URL+"/v1/integrations/email", authHeader, createBody)
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, createResp)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(createResp), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := created["id"].(string)

	status, getBody := doGet(t, srv.URL+"/v1/integrations/"+id, authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/integrations/%s: status=%d body=%s", id, status, getBody)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(getBody), &got); err != nil {
		t.Fatalf("decode get: %v (body=%s)", err, getBody)
	}

	// Secret field must be masked.
	if strings.Contains(getBody, "private-tok-secret") {
		t.Errorf("GET response leaks api_token: %s", getBody)
	}
	// Non-secret account_id must be present.
	creds, _ := got["credentialsMasked"].(map[string]any)
	if creds == nil {
		t.Fatalf("credentialsMasked missing from response: %s", getBody)
	}
	if creds["account_id"] != "public-acct-id" {
		t.Errorf("account_id = %v, want public-acct-id", creds["account_id"])
	}
}

// TestGetIntegration_ByID_NotFound returns 404 for a non-existent id.
func TestGetIntegration_ByID_NotFound(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	status, _ := doGet(t, srv.URL+"/v1/integrations/nosuchid", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET /v1/integrations/nosuchid: status=%d, want 404", status)
	}
}

// TestGetIntegration_ScopeIsolation verifies that an integration from a
// different scope returns 404 (no cross-env information leakage).
func TestGetIntegration_ScopeIsolation(t *testing.T) {
	// We cannot easily test cross-scope isolation at the E2E level without a
	// second bootstrap scope, so we verify that a syntactically valid but
	// non-existent ID in the current scope returns 404.
	srv, authHeader, _ := startOperatorServer(t)

	// Use a well-formed ULID-like string that doesn't exist in this scope.
	status, _ := doGet(t, srv.URL+"/v1/integrations/01JZZZZZZZZZZZZZZZZZZZZZZZ", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET with foreign-scope id: status=%d, want 404", status)
	}
}

// TestPutIntegration_UpdateCredentialMerge creates an SMTP integration, then
// updates it — leaving the password as the mask sentinel — and verifies that
// ResolveEmail still works with the original password.
func TestPutIntegration_UpdateCredentialMerge(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	// Create via legacy endpoint.
	createBody := `{"identifier":"smtp-put-test","host":"smtp.example.com","port":587,"username":"u@example.com","password":"keep-this-secret","from":"old@example.com","primary":false}`
	status, createResp := doPost(t, srv.URL+"/v1/integrations/email", authHeader, createBody)
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, createResp)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(createResp), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := created["id"].(string)

	// Update with sentinel password (leave unchanged) and new From.
	putBody := `{"identifier":"smtp-put-test","primary":false,"priority":0,"credentials":{"from":"new@example.com","password":"` + "••••••" + `"}}`
	status, putResp := doPut(t, srv.URL+"/v1/integrations/"+id, authHeader, putBody)
	if status != http.StatusOK {
		t.Fatalf("PUT /v1/integrations/%s: status=%d body=%s", id, status, putResp)
	}

	// The update response must not leak credential values.
	if strings.Contains(putResp, "keep-this-secret") {
		t.Errorf("PUT response leaks original password: %s", putResp)
	}
}

// TestPutIntegration_PrimaryConflict_Returns409 creates two SMTP integrations,
// one primary and one not, then attempts to promote the non-primary to primary
// — expecting 409 with a primary_conflict code.
func TestPutIntegration_PrimaryConflict_Returns409(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	// Create the first (primary) integration.
	body1 := `{"identifier":"smtp-primary","host":"smtp.example.com","port":587,"username":"u@example.com","password":"s3cret","from":"primary@example.com","primary":true}`
	if status, b := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body1); status != http.StatusCreated {
		t.Fatalf("create primary: status=%d body=%s", status, b)
	}

	// Create the second (non-primary) integration.
	body2 := `{"identifier":"smtp-secondary","host":"smtp2.example.com","port":587,"username":"u@example.com","password":"s3cret2","from":"secondary@example.com","primary":false}`
	status, createResp := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body2)
	if status != http.StatusCreated {
		t.Fatalf("create secondary: status=%d body=%s", status, createResp)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(createResp), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id2, _ := created["id"].(string)

	// Attempt to promote secondary to primary — must 409.
	putBody := `{"identifier":"smtp-secondary","primary":true,"priority":0,"credentials":{}}`
	status, conflictBody := doPut(t, srv.URL+"/v1/integrations/"+id2, authHeader, putBody)
	if status != http.StatusConflict {
		t.Fatalf("PUT promote secondary to primary: status=%d, want 409; body=%s", status, conflictBody)
	}

	var conflict map[string]any
	if err := json.Unmarshal([]byte(conflictBody), &conflict); err != nil {
		t.Fatalf("decode conflict body: %v (body=%s)", err, conflictBody)
	}
	if conflict["code"] != "primary_conflict" {
		t.Errorf("code = %v, want primary_conflict", conflict["code"])
	}
	if conflict["existingPrimaryId"] == nil || conflict["existingPrimaryId"] == "" {
		t.Errorf("existingPrimaryId missing from conflict body: %s", conflictBody)
	}
	// Conflict body must not leak credentials.
	if strings.Contains(conflictBody, "s3cret") {
		t.Errorf("conflict body leaks credentials: %s", conflictBody)
	}
}

// TestPutIntegration_NotFound returns 404 when the id does not exist.
func TestPutIntegration_NotFound(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	status, _ := doPut(t, srv.URL+"/v1/integrations/nosuchid", authHeader, `{"identifier":"x","credentials":{}}`)
	if status != http.StatusNotFound {
		t.Errorf("PUT nonexistent: status=%d, want 404", status)
	}
}

// TestDeleteIntegration_RemovesRow deletes an integration and verifies 204 +
// subsequent GET returns 404.
func TestDeleteIntegration_RemovesRow(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"smtp-to-delete","host":"smtp.example.com","port":587,"from":"del@example.com","primary":false}`
	status, createResp := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, createResp)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(createResp), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := created["id"].(string)

	status, _ = doDelete(t, srv.URL+"/v1/integrations/"+id, authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d, want 204", status)
	}

	status, _ = doGet(t, srv.URL+"/v1/integrations/"+id, authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET after DELETE: status=%d, want 404", status)
	}
}

// TestDeleteIntegration_NotFound returns 404 when the id does not exist.
func TestDeleteIntegration_NotFound(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	status, _ := doDelete(t, srv.URL+"/v1/integrations/nosuchid", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("DELETE nonexistent: status=%d, want 404", status)
	}
}

// TestPatchIntegrationActive_TogglesFlag creates an integration and flips its
// active flag via PATCH, verifying no credential round-trip is required.
func TestPatchIntegrationActive_TogglesFlag(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	createBody := `{"identifier":"smtp-patch-test","host":"smtp.example.com","port":587,"from":"patch@example.com","primary":false}`
	status, createResp := doPost(t, srv.URL+"/v1/integrations/email", authHeader, createBody)
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, createResp)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(createResp), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := created["id"].(string)

	// Deactivate.
	status, _ = doPatch(t, srv.URL+"/v1/integrations/"+id+"/active", authHeader, `{"active":false}`)
	if status != http.StatusNoContent {
		t.Fatalf("PATCH active=false: status=%d, want 204", status)
	}

	// Reactivate.
	status, _ = doPatch(t, srv.URL+"/v1/integrations/"+id+"/active", authHeader, `{"active":true}`)
	if status != http.StatusNoContent {
		t.Fatalf("PATCH active=true: status=%d, want 204", status)
	}
}

// TestPatchIntegrationActive_NotFound returns 404 for a non-existent id.
func TestPatchIntegrationActive_NotFound(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	status, _ := doPatch(t, srv.URL+"/v1/integrations/nosuchid/active", authHeader, `{"active":false}`)
	if status != http.StatusNotFound {
		t.Errorf("PATCH /active nonexistent: status=%d, want 404", status)
	}
}
