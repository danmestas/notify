package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// startOperatorServer boots a full HTTP API with authoring + connectivity
// mounted and returns the server plus an API key header and the raw scope.
func startOperatorServer(t *testing.T) (*httptest.Server, string, domain.Scope) {
	t.Helper()
	natsURL, bridgeURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Register the in_app delivery handler so the self-sufficiency proof works.
	eng.Handle("send.in_app", delivery.New(st).HandleInApp)
	eng.StartWorkers(ctx)

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	au := authoring.New(st, eng)
	conn := connectivity.New(st, masterKey)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Connectivity: conn, Authoring: au}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, scope
}

// TestWorkflowRoutes_AuthGuard proves that all three new operator routes sit
// behind identity.Middleware: no Authorization header → 401.
func TestWorkflowRoutes_AuthGuard(t *testing.T) {
	srv, _, _ := startOperatorServer(t)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/workflows", `{"name":"x","triggerIdentifier":"x","steps":[]}`},
		{http.MethodGet, "/v1/workflows", ""},
		{http.MethodPost, "/v1/integrations/email", `{"host":"smtp.example.com","from":"x@x.com"}`},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			var req *http.Request
			var err error
			if rt.body != "" {
				req, err = http.NewRequest(rt.method, srv.URL+rt.path, strings.NewReader(rt.body))
			} else {
				req, err = http.NewRequest(rt.method, srv.URL+rt.path, nil)
			}
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if rt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s without auth: status=%d, want 401", rt.method, rt.path, resp.StatusCode)
			}
		})
	}
}

// TestPostWorkflow_InvalidStepType proves that an unknown step type produces a
// 422 response whose body enumerates the allowed values.
func TestPostWorkflow_InvalidStepType(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"name":"bad","triggerIdentifier":"bad-wf","steps":[{"stepId":"S1","type":"carrier_pigeon","controlValues":{}}]}`
	status, respBody := doPost(t, srv.URL+"/v1/workflows", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", status, respBody)
	}
	// The response body must enumerate the allowed step types.
	for _, want := range []string{"in_app", "email", "push", "delay", "digest"} {
		if !strings.Contains(respBody, want) {
			t.Errorf("422 body missing allowed type %q; body: %s", want, respBody)
		}
	}
}

// TestPostWorkflow_Create proves POST /v1/workflows creates + publishes a
// workflow and returns its id + triggerIdentifier.
func TestPostWorkflow_Create(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"name":"Hello World","triggerIdentifier":"hello-world","steps":[{"stepId":"S1","type":"in_app","controlValues":{"subject":"Hi","body":"world"}}]}`
	status, respBody := doPost(t, srv.URL+"/v1/workflows", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/workflows: status=%d body=%s", status, respBody)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, respBody)
	}
	if got["id"] == "" || got["id"] == nil {
		t.Errorf("response missing id; body: %s", respBody)
	}
	if got["triggerIdentifier"] != "hello-world" {
		t.Errorf("triggerIdentifier = %v, want hello-world", got["triggerIdentifier"])
	}
}

// TestGetWorkflows proves GET /v1/workflows lists created workflows.
func TestGetWorkflows_List(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	// No workflows yet — should return empty array.
	status, respBody := doGet(t, srv.URL+"/v1/workflows", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/workflows (empty): status=%d body=%s", status, respBody)
	}
	// The response must be a JSON array (not null).
	if !strings.HasPrefix(strings.TrimSpace(respBody), "[") {
		t.Fatalf("expected JSON array, got: %s", respBody)
	}

	// Create one workflow.
	createBody := `{"name":"My Flow","triggerIdentifier":"my-flow","steps":[{"stepId":"S1","type":"in_app","controlValues":{"subject":"Hi","body":"hello"}}]}`
	if status, b := doPost(t, srv.URL+"/v1/workflows", authHeader, createBody); status != http.StatusCreated {
		t.Fatalf("create workflow: status=%d body=%s", status, b)
	}

	status, respBody = doGet(t, srv.URL+"/v1/workflows", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/workflows: status=%d body=%s", status, respBody)
	}
	var wfs []map[string]any
	if err := json.Unmarshal([]byte(respBody), &wfs); err != nil {
		t.Fatalf("decode workflows: %v (body=%s)", err, respBody)
	}
	if len(wfs) != 1 {
		t.Fatalf("len(workflows) = %d, want 1; body: %s", len(wfs), respBody)
	}
	if wfs[0]["triggerIdentifier"] != "my-flow" {
		t.Errorf("triggerIdentifier = %v, want my-flow", wfs[0]["triggerIdentifier"])
	}
	// The list DTO exposes the no-opt-out `critical` flag (API completeness).
	// The create body above omits it, so it defaults to false — but the key
	// must be present in the response shape.
	crit, ok := wfs[0]["critical"]
	if !ok {
		t.Errorf("list DTO missing `critical` key; body: %s", respBody)
	}
	if crit != false {
		t.Errorf("critical = %v, want false (create omitted it)", crit)
	}
}

// TestPostEmailIntegration_InvalidIdentifier_422 proves POST
// /v1/integrations/email rejects an identifier with URL-unsafe characters
// (same H2-class fix as the console handler): the identifier becomes a URL
// path segment for every integration route, so an unvalidated identifier
// would be created but permanently unreachable.
func TestPostEmailIntegration_InvalidIdentifier_422(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"foo/bar","host":"smtp.example.com","from":"noreply@example.com"}`
	status, resp := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (resp: %s)", status, resp)
	}
}

// TestPostEmailIntegration_PasswordNeverEchoed proves POST /v1/integrations/email
// creates the integration and does NOT echo the password back in the response.
func TestPostEmailIntegration_PasswordNeverEchoed(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"smtp-test","host":"smtp.example.com","port":587,"username":"user@example.com","password":"super-secret-password","from":"noreply@example.com","primary":true}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/email: status=%d body=%s", status, respBody)
	}

	// Response must include expected fields.
	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, respBody)
	}
	if got["id"] == nil || got["id"] == "" {
		t.Errorf("response missing id; body: %s", respBody)
	}
	if got["providerId"] != "smtp" {
		t.Errorf("providerId = %v, want smtp", got["providerId"])
	}
	if got["channel"] != "email" {
		t.Errorf("channel = %v, want email", got["channel"])
	}
	// The password must never appear in the response.
	if strings.Contains(respBody, "super-secret-password") {
		t.Errorf("response echoes the password — must not: %s", respBody)
	}
	// The enc_credentials bytes must also not appear.
	if strings.Contains(respBody, "encCredentials") || strings.Contains(respBody, "enc_credentials") {
		t.Errorf("response leaks encrypted credential bytes: %s", respBody)
	}
}

// TestPostEmailIntegration_ErrorPathRedacted forces a CreateSMTP failure (the
// integrations table has UNIQUE (environment_id, identifier), so a duplicate
// identifier errors at the store) and proves the 500 body is the fixed
// redacted message — it carries neither the password nor the host nor any
// other request detail.
func TestPostEmailIntegration_ErrorPathRedacted(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"dup-smtp","host":"smtp.secret-host.example","port":587,"username":"user@example.com","password":"super-secret-password","from":"noreply@example.com","primary":true}`
	if status, b := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body); status != http.StatusCreated {
		t.Fatalf("first create: status=%d body=%s", status, b)
	}

	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusInternalServerError {
		t.Fatalf("duplicate create: status=%d, want 500; body=%s", status, respBody)
	}
	if strings.Contains(respBody, "super-secret-password") {
		t.Errorf("error body leaks the password: %s", respBody)
	}
	if strings.Contains(respBody, "smtp.secret-host.example") {
		t.Errorf("error body leaks the host: %s", respBody)
	}
	if want := "failed to create email integration"; !strings.Contains(respBody, want) {
		t.Errorf("error body = %q, want it to contain %q", respBody, want)
	}
}

// TestPostCloudflareEmailIntegration_ProviderField proves POST
// /v1/integrations/email with provider="cloudflare-email" creates the
// integration with the correct providerId and does NOT echo the apiToken.
func TestPostCloudflareEmailIntegration_ProviderField(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"cf-test","provider":"cloudflare-email","accountId":"acct-001","apiToken":"cf-secret-token","from":"notify@example.com","primary":true}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/email (cloudflare-email): status=%d body=%s", status, respBody)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, respBody)
	}
	if got["id"] == nil || got["id"] == "" {
		t.Errorf("response missing id; body: %s", respBody)
	}
	if got["providerId"] != "cloudflare-email" {
		t.Errorf("providerId = %v, want cloudflare-email", got["providerId"])
	}
	if got["channel"] != "email" {
		t.Errorf("channel = %v, want email", got["channel"])
	}
	// apiToken must never appear in the response.
	if strings.Contains(respBody, "cf-secret-token") {
		t.Errorf("response echoes the apiToken — must not: %s", respBody)
	}
	if strings.Contains(respBody, "encCredentials") || strings.Contains(respBody, "enc_credentials") {
		t.Errorf("response leaks encrypted credential bytes: %s", respBody)
	}
}

// TestPostCloudflareEmailIntegration_ErrorPathRedacted proves the 500 error
// body for a cloudflare-email duplicate identifier does NOT leak the apiToken
// or accountId.
func TestPostCloudflareEmailIntegration_ErrorPathRedacted(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"dup-cf","provider":"cloudflare-email","accountId":"secret-account-id","apiToken":"cf-secret-token","from":"notify@example.com","primary":true}`
	if status, b := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body); status != http.StatusCreated {
		t.Fatalf("first create: status=%d body=%s", status, b)
	}

	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusInternalServerError {
		t.Fatalf("duplicate create: status=%d, want 500; body=%s", status, respBody)
	}
	if strings.Contains(respBody, "cf-secret-token") {
		t.Errorf("error body leaks the apiToken: %s", respBody)
	}
	if strings.Contains(respBody, "secret-account-id") {
		t.Errorf("error body leaks the accountId: %s", respBody)
	}
	if want := "failed to create email integration"; !strings.Contains(respBody, want) {
		t.Errorf("error body = %q, want it to contain %q", respBody, want)
	}
}

// TestPostEmailIntegration_SMTPDefaultProvider proves that omitting the
// "provider" field (or setting it to "smtp") produces providerId="smtp" and
// the existing SMTP body works unchanged.
func TestPostEmailIntegration_SMTPDefaultProvider(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	// No provider field — should default to smtp.
	body := `{"identifier":"smtp-default","host":"smtp.example.com","port":587,"username":"user@example.com","password":"secret","from":"noreply@example.com","primary":false}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/email (smtp default): status=%d body=%s", status, respBody)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["providerId"] != "smtp" {
		t.Errorf("providerId = %v, want smtp (default)", got["providerId"])
	}
	if strings.Contains(respBody, "secret") {
		t.Errorf("response echoes the password — must not: %s", respBody)
	}
}

// TestPostEmailIntegration_UnknownProviderRejected proves an unrecognised
// "provider" value fails closed with 400 and an error body enumerating the
// allowed providers — it must not be stored as a malformed SMTP row.
func TestPostEmailIntegration_UnknownProviderRejected(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	body := `{"identifier":"sg-test","provider":"sendgrid","host":"smtp.sendgrid.net","from":"noreply@example.com","primary":false}`
	status, respBody := doPost(t, srv.URL+"/v1/integrations/email", authHeader, body)
	if status != http.StatusBadRequest {
		t.Fatalf("POST /v1/integrations/email (provider=sendgrid): status=%d, want 400; body=%s", status, respBody)
	}
	for _, want := range []string{"unknown provider", "smtp", "cloudflare-email"} {
		if !strings.Contains(respBody, want) {
			t.Errorf("400 body missing %q; body: %s", want, respBody)
		}
	}
}

// TestOperatorAPI_SelfSufficiency is the pure-HTTP self-sufficiency proof:
// create an in_app workflow entirely via HTTP, trigger via HTTP, and assert the
// message row reaches "sent" status — no in-process service calls after
// startOperatorServer. Uses the in-process in_app channel so there are no
// external dependencies.
func TestOperatorAPI_SelfSufficiency(t *testing.T) {
	srv, authHeader, _ := startOperatorServer(t)

	// Step 1 — Create subscriber entirely via HTTP.
	subBody := `{"subscriberId":"op-user-1","firstName":"Operator"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// Step 2 — Create + publish the workflow entirely via HTTP.
	wfBody := `{"name":"Operator Welcome","triggerIdentifier":"op-welcome","steps":[{"stepId":"S1","type":"in_app","controlValues":{"subject":"Hi {{subscriber.firstName}}","body":"HTTP-self-sufficient"}}]}`
	if status, body := doPost(t, srv.URL+"/v1/workflows", authHeader, wfBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/workflows: status=%d body=%s", status, body)
	}

	// Step 3 — Trigger via HTTP.
	trigBody := `{"name":"op-welcome","to":{"subscriberId":"op-user-1"},"payload":{},"transactionId":"op-tx-1"}`
	trigStatus, trigResp := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if trigStatus != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", trigStatus, trigResp)
	}

	// Step 4 — Poll GET /v1/messages until the in_app message arrives with status="sent".
	feedURL := srv.URL + "/v1/messages?subscriberId=op-user-1&channel=in_app"
	var msgs []domain.Message
	var lastBody string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, body := doGet(t, feedURL, authHeader)
		lastBody = body
		if status != http.StatusOK {
			t.Fatalf("GET /v1/messages: status=%d body=%s", status, body)
		}
		var got []domain.Message
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode messages: %v (body=%s)", err, body)
		}
		if len(got) >= 1 {
			msgs = got
			break
		}
		t.Logf("poll: 0 messages yet (feed=%s)", body)
		time.Sleep(300 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatalf("self-sufficiency: no in_app message within 20s; last feed: %s", lastBody)
	}

	msg := msgs[0]
	if msg.Status != "sent" {
		t.Errorf("Status = %q, want sent", msg.Status)
	}
	if msg.Subject != "Hi Operator" {
		t.Errorf("Subject = %q, want \"Hi Operator\"", msg.Subject)
	}
	if msg.Content != "HTTP-self-sufficient" {
		t.Errorf("Content = %q, want \"HTTP-self-sufficient\"", msg.Content)
	}
	if msg.TransactionID != "op-tx-1" {
		t.Errorf("TransactionID = %q, want op-tx-1", msg.TransactionID)
	}
}
