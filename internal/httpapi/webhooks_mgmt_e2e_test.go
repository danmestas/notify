package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/identity"
)

// TestWebhookEndpoint_AuthGuard verifies all webhook-endpoint management routes
// require auth.
func TestWebhookEndpoint_AuthGuard(t *testing.T) {
	srv, _, _, _, _ := startConsoleServer(t)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/webhooks/endpoints", ""},
		{http.MethodPost, "/v1/webhooks/endpoints", `{"url":"https://a.com"}`},
		{http.MethodGet, "/v1/webhooks/endpoints/some-id", ""},
		{http.MethodPut, "/v1/webhooks/endpoints/some-id", `{"url":"https://b.com"}`},
		{http.MethodDelete, "/v1/webhooks/endpoints/some-id", ""},
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
		}
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: status=%d, want 401", rt.method, rt.path, status)
		}
	}
}

// TestWebhookEndpoint_ListEmpty returns an empty data array when no endpoints
// exist.
func TestWebhookEndpoint_ListEmpty(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/webhooks/endpoints", authHeader)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}
	var resp struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(resp.Data) != 0 {
		t.Errorf("expected empty list, got %d items", len(resp.Data))
	}
}

// TestWebhookEndpoint_CRUD exercises the full create/read/update/delete cycle.
func TestWebhookEndpoint_CRUD(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	// POST creates a new endpoint.
	status, body := doPost(t, srv.URL+"/v1/webhooks/endpoints", authHeader, `{"url":"https://example.com/hook","active":true}`)
	if status != http.StatusCreated {
		t.Fatalf("POST expected 201, got %d body=%s", status, body)
	}
	var created struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode create: %v (body=%s)", err, body)
	}
	if created.ID == "" {
		t.Fatalf("expected non-empty id")
	}
	if created.URL != "https://example.com/hook" {
		t.Errorf("url = %q, want https://example.com/hook", created.URL)
	}
	if !created.Active {
		t.Errorf("expected active=true")
	}

	// GET returns the created endpoint.
	status, body = doGet(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID, authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET expected 200, got %d body=%s", status, body)
	}
	var got struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode get: %v (body=%s)", err, body)
	}
	if got.ID != created.ID {
		t.Errorf("id = %q, want %q", got.ID, created.ID)
	}

	// GET /v1/webhooks/endpoints lists all endpoints (active + inactive).
	status, body = doGet(t, srv.URL+"/v1/webhooks/endpoints", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list expected 200, got %d body=%s", status, body)
	}
	var listResp struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, body)
	}
	if len(listResp.Data) != 1 {
		t.Errorf("expected 1 endpoint in list, got %d", len(listResp.Data))
	}

	// PUT updates the URL and toggles active.
	putActive := false
	putBody, _ := json.Marshal(map[string]any{"url": "https://new.example.com/hook", "active": putActive})
	status, body = doPut(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID, authHeader, string(putBody))
	if status != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d body=%s", status, body)
	}
	var updated struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("decode put: %v (body=%s)", err, body)
	}
	if updated.URL != "https://new.example.com/hook" {
		t.Errorf("updated url = %q, want https://new.example.com/hook", updated.URL)
	}
	if updated.Active {
		t.Errorf("expected active=false after update")
	}

	// DELETE removes the endpoint.
	status, _ = doDelete(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID, authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d", status)
	}

	// GET after delete → 404.
	status, _ = doGet(t, srv.URL+"/v1/webhooks/endpoints/"+created.ID, authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", status)
	}
}

// TestWebhookEndpoint_NotFound verifies 404 responses for unknown ids.
func TestWebhookEndpoint_NotFound(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	if status, _ := doGet(t, srv.URL+"/v1/webhooks/endpoints/nope", authHeader); status != http.StatusNotFound {
		t.Errorf("GET unknown = %d, want 404", status)
	}
	if status, _ := doPut(t, srv.URL+"/v1/webhooks/endpoints/nope", authHeader, `{"url":"https://x.com"}`); status != http.StatusNotFound {
		t.Errorf("PUT unknown = %d, want 404", status)
	}
	if status, _ := doDelete(t, srv.URL+"/v1/webhooks/endpoints/nope", authHeader); status != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", status)
	}
}

// TestWebhookEndpoint_PostValidation verifies that POST rejects missing url.
func TestWebhookEndpoint_PostValidation(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, _ := doPost(t, srv.URL+"/v1/webhooks/endpoints", authHeader, `{}`)
	if status != http.StatusBadRequest {
		t.Errorf("POST missing url = %d, want 400", status)
	}
}

// TestWebhookEndpoint_List_IncludesInactive verifies that GET /v1/webhooks/endpoints
// returns both active and inactive endpoints, unlike the internal ListActiveByEnv.
func TestWebhookEndpoint_List_IncludesInactive(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	doPost(t, srv.URL+"/v1/webhooks/endpoints", authHeader, `{"url":"https://active.com","active":true}`)
	doPost(t, srv.URL+"/v1/webhooks/endpoints", authHeader, `{"url":"https://inactive.com","active":false}`)

	status, body := doGet(t, srv.URL+"/v1/webhooks/endpoints", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list: %d body=%s", status, body)
	}
	var resp struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Errorf("expected 2 endpoints (active + inactive), got %d", len(resp.Data))
	}
}

// TestWebhookEndpoint_CrossEnvIsolation verifies that webhook endpoints created
// in one env are not visible from another.
func TestWebhookEndpoint_CrossEnvIsolation(t *testing.T) {
	ctx := context.Background()
	srv, authA, _, _, st := startConsoleServer(t)

	// Create an endpoint under scope A.
	status, body := doPost(t, srv.URL+"/v1/webhooks/endpoints", authA, `{"url":"https://a.com","active":true}`)
	if status != http.StatusCreated {
		t.Fatalf("POST: %d body=%s", status, body)
	}
	var ep struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &ep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Bootstrap scope B.
	masterKey := bytes.Repeat([]byte{0x2f}, 32)
	idB := identity.New(st, masterKey)
	keyB, _, err := idB.Bootstrap(ctx, "AcmeB", "prod")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authB := "ApiKey " + keyB

	// Scope B cannot see or delete scope A's endpoint.
	if status, _ := doGet(t, srv.URL+"/v1/webhooks/endpoints/"+ep.ID, authB); status != http.StatusNotFound {
		t.Errorf("cross-env get = %d, want 404", status)
	}
	if status, _ := doDelete(t, srv.URL+"/v1/webhooks/endpoints/"+ep.ID, authB); status != http.StatusNotFound {
		t.Errorf("cross-env delete = %d, want 404", status)
	}

	// Scope B list returns 0, scope A list returns 1.
	_, bodyB := doGet(t, srv.URL+"/v1/webhooks/endpoints", authB)
	var respB struct {
		Data []any `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyB), &respB)
	if len(respB.Data) != 0 {
		t.Errorf("scopeB list = %d, want 0", len(respB.Data))
	}

	_, bodyA := doGet(t, srv.URL+"/v1/webhooks/endpoints", authA)
	var respA struct {
		Data []any `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyA), &respA)
	if len(respA.Data) != 1 {
		t.Errorf("scopeA list = %d, want 1", len(respA.Data))
	}
}
