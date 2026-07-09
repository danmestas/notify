package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// setupWorkflowSuite spins up a complete stack (sidecar, store, services,
// httptest.Server) and creates+publishes a single in_app workflow. It returns
// the server, auth header, and the trigger identifier of the created workflow.
func setupWorkflowSuite(t *testing.T) (srv *httptest.Server, authHeader string, svc *authoring.Service, sc domain.Scope) {
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

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv = httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, auth))
	t.Cleanup(srv.Close)

	return srv, "ApiKey " + key, auth, scope
}

// TestPutWorkflow_UpdatesNameAndSteps verifies PUT /v1/workflows/{slug}
// returns 200 with the updated workflow and the change is reflected on
// subsequent GET.
func TestPutWorkflow_UpdatesNameAndSteps(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	// Create via service directly (mirrors existing test patterns).
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Original",
		TriggerIdentifier: "put-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"v1"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "put-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	body := `{"name":"Updated","steps":[{"stepId":"S1","type":"in_app","controlValues":{"body":"v2"}}]}`
	status, resp := doPut(t, srv.URL+"/v1/workflows/put-wf", authHeader, body)
	if status != http.StatusOK {
		t.Fatalf("PUT /v1/workflows/put-wf: status=%d body=%s", status, resp)
	}

	var dto map[string]any
	if err := json.Unmarshal([]byte(resp), &dto); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, resp)
	}
	if dto["name"] != "Updated" {
		t.Errorf("name = %v, want Updated", dto["name"])
	}
	if dto["triggerIdentifier"] != "put-wf" {
		t.Errorf("triggerIdentifier = %v, want put-wf (must not change)", dto["triggerIdentifier"])
	}

	// GET should reflect the update.
	getStatus, getBody := doGet(t, srv.URL+"/v1/workflows/put-wf", authHeader)
	if getStatus != http.StatusOK {
		t.Fatalf("GET /v1/workflows/put-wf: status=%d body=%s", getStatus, getBody)
	}
	var gotDTO map[string]any
	if err := json.Unmarshal([]byte(getBody), &gotDTO); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if gotDTO["name"] != "Updated" {
		t.Errorf("GET name = %v, want Updated", gotDTO["name"])
	}
}

// TestPutWorkflow_404ForMissing verifies PUT returns 404 for an unknown slug.
func TestPutWorkflow_404ForMissing(t *testing.T) {
	srv, authHeader, _, _ := setupWorkflowSuite(t)

	body := `{"name":"X","steps":[{"stepId":"S1","type":"in_app"}]}`
	status, _ := doPut(t, srv.URL+"/v1/workflows/no-such", authHeader, body)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

// TestPutWorkflow_422OnBadDelayStep verifies PUT returns 422 when a delay step
// has a non-positive amount (compile failure).
func TestPutWorkflow_422OnBadDelayStep(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "WF",
		TriggerIdentifier: "wf-422",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "wf-422"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	body := `{"name":"Bad","steps":[{"stepId":"d1","type":"delay","controlValues":{"amount":0,"unit":"seconds"}}]}`
	status, _ := doPut(t, srv.URL+"/v1/workflows/wf-422", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", status)
	}
}

// TestPutWorkflow_AuthGuard verifies PUT /v1/workflows/{slug} requires auth.
func TestPutWorkflow_AuthGuard(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "WF",
		TriggerIdentifier: "wf-auth",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "wf-auth"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	_ = authHeader // confirm it compiles

	body := `{"name":"X","steps":[]}`
	status, _ := doPut(t, srv.URL+"/v1/workflows/wf-auth", "ApiKey bad-key", body)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad key, got %d", status)
	}
}

// TestDeleteWorkflow_RemovesWorkflow verifies DELETE /v1/workflows/{slug}
// returns 204 and the workflow can no longer be found.
func TestDeleteWorkflow_RemovesWorkflow(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "ToDelete",
		TriggerIdentifier: "del-http-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "del-http-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	status, body := doDelete(t, srv.URL+"/v1/workflows/del-http-wf", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/workflows/del-http-wf: status=%d body=%s", status, body)
	}

	// GET must return 404.
	getStatus, _ := doGet(t, srv.URL+"/v1/workflows/del-http-wf", authHeader)
	if getStatus != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", getStatus)
	}
}

// TestDeleteWorkflow_404ForMissing verifies DELETE returns 404 for unknown slug.
func TestDeleteWorkflow_404ForMissing(t *testing.T) {
	srv, authHeader, _, _ := setupWorkflowSuite(t)

	status, _ := doDelete(t, srv.URL+"/v1/workflows/no-such-wf", authHeader)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

// TestPatchWorkflowActivate_RoundTrip verifies PATCH .../activate toggles
// active and the trigger service skips inactive workflows.
func TestPatchWorkflowActivate_RoundTrip(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	// Create a subscriber and workflow first.
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Toggle",
		TriggerIdentifier: "toggle-http-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "toggle-http-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Deactivate via PATCH.
	status, resp := doPatch(t, srv.URL+"/v1/workflows/toggle-http-wf/activate", authHeader, `{"active":false}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH activate false: status=%d body=%s", status, resp)
	}
	var dto map[string]any
	if err := json.Unmarshal([]byte(resp), &dto); err != nil {
		t.Fatalf("decode PATCH response: %v", err)
	}
	if dto["active"] != false {
		t.Errorf("active = %v, want false", dto["active"])
	}

	// Re-activate via PATCH.
	status, resp = doPatch(t, srv.URL+"/v1/workflows/toggle-http-wf/activate", authHeader, `{"active":true}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH activate true: status=%d body=%s", status, resp)
	}
	if err := json.Unmarshal([]byte(resp), &dto); err != nil {
		t.Fatalf("decode PATCH response: %v", err)
	}
	if dto["active"] != true {
		t.Errorf("active = %v, want true", dto["active"])
	}
}

// TestPatchWorkflowActivate_404ForMissing verifies PATCH activate returns 404
// for an unknown slug.
func TestPatchWorkflowActivate_404ForMissing(t *testing.T) {
	srv, authHeader, _, _ := setupWorkflowSuite(t)

	status, _ := doPatch(t, srv.URL+"/v1/workflows/no-such/activate", authHeader, `{"active":false}`)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

// TestTriggerSkipsInactiveWorkflow verifies that trigger.Ingest no-ops cleanly
// when a workflow has active=false (spec-02 B4 requirement).
func TestTriggerSkipsInactiveWorkflow(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "InactiveWF",
		TriggerIdentifier: "inactive-wf",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hi"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "inactive-wf"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Deactivate via PATCH.
	if status, body := doPatch(t, srv.URL+"/v1/workflows/inactive-wf/activate", authHeader, `{"active":false}`); status != http.StatusOK {
		t.Fatalf("PATCH deactivate: status=%d body=%s", status, body)
	}

	// Trigger must return acknowledged=false with a status of "error".
	subBody := `{"subscriberId":"u1","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST subscribers: status=%d body=%s", status, body)
	}
	trigBody := `{"name":"inactive-wf","to":{"subscriberId":"u1"},"payload":{}}`
	trigStatus, trigResp := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if trigStatus != http.StatusCreated {
		t.Fatalf("POST trigger: status=%d body=%s", trigStatus, trigResp)
	}
	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(trigResp), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v", err)
	}
	if resp.Acknowledged {
		t.Errorf("expected acknowledged=false for inactive workflow, got true")
	}
	if resp.Status != "error" {
		t.Errorf("expected status=error for inactive workflow, got %q", resp.Status)
	}
}

// TestPostWorkflowDuplicate_CopiesWorkflow verifies POST .../duplicate creates
// a new workflow inheriting the source steps.
func TestPostWorkflowDuplicate_CopiesWorkflow(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Source",
		TriggerIdentifier: "dup-src-http",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"hello"}`)}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "dup-src-http"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	body := `{"name":"Source (Copy)","triggerIdentifier":"dup-copy-http"}`
	status, resp := doPost(t, srv.URL+"/v1/workflows/dup-src-http/duplicate", authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("POST duplicate: status=%d body=%s", status, resp)
	}

	var dto map[string]any
	if err := json.Unmarshal([]byte(resp), &dto); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if dto["triggerIdentifier"] != "dup-copy-http" {
		t.Errorf("triggerIdentifier = %v, want dup-copy-http", dto["triggerIdentifier"])
	}
	if dto["name"] != "Source (Copy)" {
		t.Errorf("name = %v, want 'Source (Copy)'", dto["name"])
	}

	// Source must still exist.
	srcStatus, _ := doGet(t, srv.URL+"/v1/workflows/dup-src-http", authHeader)
	if srcStatus != http.StatusOK {
		t.Fatalf("source workflow gone after duplicate, status=%d", srcStatus)
	}

	// Duplicate workflow must be reachable.
	copyStatus, _ := doGet(t, srv.URL+"/v1/workflows/dup-copy-http", authHeader)
	if copyStatus != http.StatusOK {
		t.Fatalf("duplicated workflow not found, status=%d", copyStatus)
	}
}

// TestPostWorkflowDuplicate_409OnCollision verifies POST duplicate returns 409
// when the desired triggerIdentifier already exists.
func TestPostWorkflowDuplicate_409OnCollision(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	for _, slug := range []string{"col-src", "col-existing"} {
		if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
			Name:              slug,
			TriggerIdentifier: slug,
			Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
		}); err != nil {
			t.Fatalf("Create %s: %v", slug, err)
		}
		if err := auth.Publish(ctx, scope, slug); err != nil {
			t.Fatalf("Publish %s: %v", slug, err)
		}
	}

	body := `{"name":"Collision","triggerIdentifier":"col-existing"}`
	status, resp := doPost(t, srv.URL+"/v1/workflows/col-src/duplicate", authHeader, body)
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body=%s)", status, resp)
	}
}

// TestPostWorkflowDuplicate_404ForMissingSrc verifies POST duplicate returns
// 404 when the source workflow does not exist.
func TestPostWorkflowDuplicate_404ForMissingSrc(t *testing.T) {
	srv, authHeader, _, _ := setupWorkflowSuite(t)

	body := `{"name":"X","triggerIdentifier":"new-id"}`
	status, _ := doPost(t, srv.URL+"/v1/workflows/no-src/duplicate", authHeader, body)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

// TestPostWorkflow_409OnDuplicateTrigger verifies POST /v1/workflows returns
// 409 when the triggerIdentifier already exists in this scope.
func TestPostWorkflow_409OnDuplicateTrigger(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, auth, scope := setupWorkflowSuite(t)

	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Existing",
		TriggerIdentifier: "dup-trigger",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app"}},
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}

	body := `{"name":"Duplicate","triggerIdentifier":"dup-trigger","steps":[{"stepId":"S1","type":"in_app"}]}`
	status, resp := doPost(t, srv.URL+"/v1/workflows", authHeader, body)
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d (body=%s)", status, resp)
	}
}

// TestPostWorkflow_422OnInvalidDelayStep verifies POST /v1/workflows returns
// 422 when a delay step has a non-positive amount.
func TestPostWorkflow_422OnInvalidDelayStep(t *testing.T) {
	srv, authHeader, _, _ := setupWorkflowSuite(t)

	body := `{"name":"BadDelay","triggerIdentifier":"bad-delay-post","steps":[{"stepId":"d1","type":"delay","controlValues":{"amount":0,"unit":"seconds"}}]}`
	status, resp := doPost(t, srv.URL+"/v1/workflows", authHeader, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (body=%s)", status, resp)
	}

	// The invalid create must NOT leak a persisted workflow row: validation
	// happens (via Compile) before any write. GET the workflow back → 404.
	getStatus, getResp := doGet(t, srv.URL+"/v1/workflows/bad-delay-post", authHeader)
	if getStatus != http.StatusNotFound {
		t.Fatalf("invalid create leaked a workflow row: GET returned %d (body=%s), want 404", getStatus, getResp)
	}
}
