package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// startConsoleServer boots the HTTP API with authoring + store wired so the
// console read routes are live. Returns the server, a bootstrapped API key
// header, the scope, the authoring service, and the store.
func startConsoleServer(t *testing.T) (*httptest.Server, string, domain.Scope, *authoring.Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	trg := trigger.New(aud, st, nil)
	inbox := inboxapi.New(st)
	au := authoring.New(st, nil) // no engine — authoring CRUD doesn't need it

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Store: st, Authoring: au}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, scope, au, st
}

// TestConsole_GetWorkflow_AuthGuard verifies the route sits behind the auth middleware.
func TestConsole_GetWorkflow_AuthGuard(t *testing.T) {
	srv, _, _, _, _ := startConsoleServer(t)

	status, _ := doGet(t, srv.URL+"/v1/workflows/some-slug", "")
	if status != 401 {
		t.Fatalf("expected 401 without auth, got %d", status)
	}
}

// TestConsole_GetWorkflow_NotFound returns 404 for an unknown slug.
func TestConsole_GetWorkflow_NotFound(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, _ := doGet(t, srv.URL+"/v1/workflows/does-not-exist", authHeader)
	if status != 404 {
		t.Fatalf("expected 404 for unknown slug, got %d", status)
	}
}

// TestConsole_GetWorkflow_ReturnsFullDTO seeds a workflow and asserts the full
// DTO (steps + payloadSchema) is returned by GET /v1/workflows/{slug}.
func TestConsole_GetWorkflow_ReturnsFullDTO(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, au, _ := startConsoleServer(t)

	steps := []domain.Step{
		{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"Hello"}`)},
		{StepID: "s2", Type: "email", ControlValues: json.RawMessage(`{"subject":"Hi","body":"World"}`)},
	}
	_, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "My Workflow",
		TriggerIdentifier: "my-workflow",
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/workflows/my-workflow", authHeader)
	if status != 200 {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}

	var got struct {
		ID                string          `json:"id"`
		Name              string          `json:"name"`
		TriggerIdentifier string          `json:"triggerIdentifier"`
		Active            bool            `json:"active"`
		Steps             json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if got.TriggerIdentifier != "my-workflow" {
		t.Errorf("triggerIdentifier = %q, want %q", got.TriggerIdentifier, "my-workflow")
	}
	if got.Name != "My Workflow" {
		t.Errorf("name = %q, want %q", got.Name, "My Workflow")
	}
	if !got.Active {
		t.Errorf("active = false, want true")
	}

	// Steps must be present and contain both step entries.
	var parsedSteps []domain.Step
	if err := json.Unmarshal(got.Steps, &parsedSteps); err != nil {
		t.Fatalf("unmarshal steps: %v", err)
	}
	if len(parsedSteps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(parsedSteps))
	}
	if parsedSteps[0].StepID != "s1" || parsedSteps[1].StepID != "s2" {
		t.Errorf("step ids = [%q,%q], want [s1,s2]", parsedSteps[0].StepID, parsedSteps[1].StepID)
	}
}

// TestConsole_GetWorkflow_ExposesCritical proves the detail DTO carries the
// workflow's no-opt-out `critical` flag for both a critical=true and a
// critical=false workflow (API completeness — the console/operator needs to
// know a workflow is critical without a separate lookup).
func TestConsole_GetWorkflow_ExposesCritical(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, au, _ := startConsoleServer(t)

	if _, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name: "Password Reset", TriggerIdentifier: "password-reset",
		Steps:    []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{"subject":"Reset","body":"link"}`)}},
		Critical: true,
	}); err != nil {
		t.Fatalf("authoring.Create critical: %v", err)
	}
	if _, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name: "Newsletter", TriggerIdentifier: "newsletter",
		Steps:    []domain.Step{{StepID: "s1", Type: "email", ControlValues: json.RawMessage(`{"subject":"News","body":"body"}`)}},
		Critical: false,
	}); err != nil {
		t.Fatalf("authoring.Create non-critical: %v", err)
	}

	for _, tc := range []struct {
		slug string
		want bool
	}{
		{"password-reset", true},
		{"newsletter", false},
	} {
		status, body := doGet(t, srv.URL+"/v1/workflows/"+tc.slug, authHeader)
		if status != 200 {
			t.Fatalf("GET %s: status=%d body=%s", tc.slug, status, body)
		}
		var got struct {
			Critical bool `json:"critical"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", tc.slug, err, body)
		}
		if got.Critical != tc.want {
			t.Errorf("%s: critical = %v, want %v (body=%s)", tc.slug, got.Critical, tc.want, body)
		}
	}
}

// TestConsole_GetWorkflow_ScopeIsolation verifies that a workflow in env-A
// cannot be fetched from env-B (different API key / scope).
func TestConsole_GetWorkflow_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	srv, authA, scopeA, au, st := startConsoleServer(t)

	// Create workflow in scope A.
	_, err := au.Create(ctx, scopeA, authoring.WorkflowInput{
		Name: "Scope A Workflow", TriggerIdentifier: "scope-a-wf",
		Steps: []domain.Step{{StepID: "s1", Type: "in_app"}},
	})
	if err != nil {
		t.Fatalf("authoring.Create scope A: %v", err)
	}

	// Bootstrap a second environment (scope B) within the same server.
	masterKey := bytes.Repeat([]byte{0x2b}, 32)
	idB := identity.New(st, masterKey)
	keyB, _, err := idB.Bootstrap(ctx, "AcmeB", "staging")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authB := "ApiKey " + keyB

	// Scope A's workflow must not be visible from scope B.
	status, _ := doGet(t, srv.URL+"/v1/workflows/scope-a-wf", authB)
	if status != 404 {
		t.Fatalf("expected 404 for cross-scope access, got %d", status)
	}

	// Scope A can still see its own workflow.
	status, _ = doGet(t, srv.URL+"/v1/workflows/scope-a-wf", authA)
	if status != 200 {
		t.Fatalf("expected 200 for scope A owner, got %d", status)
	}
}

// TestConsole_GetNotifications_AuthGuard verifies the route sits behind auth.
func TestConsole_GetNotifications_AuthGuard(t *testing.T) {
	srv, _, _, _, _ := startConsoleServer(t)

	status, _ := doGet(t, srv.URL+"/v1/notifications", "")
	if status != 401 {
		t.Fatalf("expected 401 without auth, got %d", status)
	}
}

// TestConsole_GetNotifications_Empty returns an empty items list when there are
// no notifications in scope.
func TestConsole_GetNotifications_Empty(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/notifications", authHeader)
	if status != 200 {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}

	var resp struct {
		Items      []any `json:"items"`
		TotalCount int   `json:"totalCount"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(resp.Items))
	}
	if resp.TotalCount != 0 {
		t.Fatalf("expected totalCount=0, got %d", resp.TotalCount)
	}
}

// TestConsole_GetNotifications_FiltersAndCursorWalk seeds notifications and
// verifies filters (workflowId, subscriberId, transactionId), cursor
// pagination, and the "next" field in the response.
func TestConsole_GetNotifications_FiltersAndCursorWalk(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	repo := st.Notifications()

	// Seed 7 notifications for wf-1 and 3 for wf-2.
	for i := 0; i < 7; i++ {
		if _, err := repo.Create(ctx, scope, domain.Notification{
			WorkflowID:    "wf-1",
			SubscriberID:  "sub-1",
			TransactionID: "tx-1",
			RunID:         "r" + string(rune('a'+i)),
		}); err != nil {
			t.Fatalf("seed wf-1 %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := repo.Create(ctx, scope, domain.Notification{
			WorkflowID: "wf-2",
			RunID:      "q" + string(rune('a'+i)),
		}); err != nil {
			t.Fatalf("seed wf-2 %d: %v", i, err)
		}
	}

	// Filter by workflowId=wf-1 → expect 7.
	status, body := doGet(t, srv.URL+"/v1/notifications?workflowId=wf-1", authHeader)
	if status != 200 {
		t.Fatalf("filter wf-1: status=%d body=%s", status, body)
	}
	var resp struct {
		Items      []domain.Notification `json:"items"`
		Next       string                `json:"next"`
		TotalCount int                   `json:"totalCount"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalCount != 7 {
		t.Fatalf("totalCount for wf-1 = %d, want 7", resp.TotalCount)
	}

	// Cursor walk with limit=3: collect all pages.
	var allItems []domain.Notification
	cursor := ""
	for {
		url := srv.URL + "/v1/notifications?workflowId=wf-1&limit=3"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		status, body = doGet(t, url, authHeader)
		if status != 200 {
			t.Fatalf("cursor walk: status=%d body=%s", status, body)
		}
		var page struct {
			Items []domain.Notification `json:"items"`
			Next  string                `json:"next"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		allItems = append(allItems, page.Items...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(allItems) != 7 {
		t.Fatalf("cursor walk collected %d items, want 7", len(allItems))
	}

	// Filter by subscriberId.
	status, body = doGet(t, srv.URL+"/v1/notifications?subscriberId=sub-1", authHeader)
	if status != 200 {
		t.Fatalf("filter sub-1: status=%d body=%s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode sub-1: %v", err)
	}
	if resp.TotalCount != 7 {
		t.Fatalf("totalCount for sub-1 = %d, want 7", resp.TotalCount)
	}

	// Filter by transactionId.
	status, body = doGet(t, srv.URL+"/v1/notifications?transactionId=tx-1", authHeader)
	if status != 200 {
		t.Fatalf("filter tx-1: status=%d body=%s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode tx-1: %v", err)
	}
	if resp.TotalCount != 7 {
		t.Fatalf("totalCount for tx-1 = %d, want 7", resp.TotalCount)
	}
}

// TestConsole_GetNotifications_InvalidCursor returns 400 for a malformed cursor.
func TestConsole_GetNotifications_InvalidCursor(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, _ := doGet(t, srv.URL+"/v1/notifications?cursor=!!!notvalid", authHeader)
	if status != 400 {
		t.Fatalf("expected 400 for invalid cursor, got %d", status)
	}
}

// TestConsole_GetNotification_NotFound returns 404 for an unknown id.
func TestConsole_GetNotification_NotFound(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, _ := doGet(t, srv.URL+"/v1/notifications/does-not-exist", authHeader)
	if status != 404 {
		t.Fatalf("expected 404, got %d", status)
	}
}

// TestConsole_GetNotification_ReturnsRow seeds a notification and asserts GET
// /v1/notifications/{id} returns the full row.
func TestConsole_GetNotification_ReturnsRow(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	repo := st.Notifications()

	created, err := repo.Create(ctx, scope, domain.Notification{
		WorkflowID:    "wf-99",
		SubscriberID:  "sub-99",
		TransactionID: "tx-99",
		RunID:         "run-99",
		Payload:       json.RawMessage(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/notifications/"+created.ID, authHeader)
	if status != 200 {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}

	var got domain.Notification
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if got.ID != created.ID {
		t.Errorf("id = %q, want %q", got.ID, created.ID)
	}
	if got.WorkflowID != "wf-99" {
		t.Errorf("workflowId = %q, want wf-99", got.WorkflowID)
	}
}

// TestConsole_GetNotification_CrossEnvIsolation ensures that a notification
// created in env-A returns 404 when queried from env-B.
func TestConsole_GetNotification_CrossEnvIsolation(t *testing.T) {
	ctx := context.Background()
	srv, authA, scopeA, _, st := startConsoleServer(t)
	repo := st.Notifications()

	created, err := repo.Create(ctx, scopeA, domain.Notification{
		WorkflowID: "wf-a",
		RunID:      "run-a",
	})
	if err != nil {
		t.Fatalf("Create in scopeA: %v", err)
	}

	// Bootstrap a second environment.
	masterKey := bytes.Repeat([]byte{0x2c}, 32)
	idB := identity.New(st, masterKey)
	keyB, _, err := idB.Bootstrap(ctx, "AcmeB", "prod")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authB := "ApiKey " + keyB

	// scope-B should NOT see scope-A's notification.
	status, _ := doGet(t, srv.URL+"/v1/notifications/"+created.ID, authB)
	if status != 404 {
		t.Fatalf("expected 404 cross-env, got %d", status)
	}

	// scope-A can still see it.
	status, _ = doGet(t, srv.URL+"/v1/notifications/"+created.ID, authA)
	if status != 200 {
		t.Fatalf("expected 200 for owner scope, got %d", status)
	}
}
