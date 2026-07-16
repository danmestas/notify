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

	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
)

// startContextServer boots a temp-SQLite store, mints one tenant, and mounts
// the real HTTP surface. The contexts service auto-wires from the store inside
// httpapi.New; inbox is the only non-nil-guarded dependency so it is supplied
// (no engine required for the in-app feed surface).
func startContextServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	id := identity.New(st, make([]byte, 32))
	key, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	inbox := inboxapi.New(st)
	handler := httpapi.New(id, nil, nil, inbox, nil, nil, nil, nil, nil, nil, nil, st, nil, nil, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key
}

func TestContexts_AuthGuard(t *testing.T) {
	srv, _ := startContextServer(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/contexts"},
		{http.MethodPost, "/v1/contexts"},
		{http.MethodGet, "/v1/contexts/tenant/acme"},
		{http.MethodPut, "/v1/contexts/tenant/acme"},
		{http.MethodDelete, "/v1/contexts/tenant/acme"},
	} {
		req, _ := http.NewRequest(route.method, srv.URL+route.path, bytes.NewReader([]byte("{}")))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", route.method, route.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without key: status=%d, want 401", route.method, route.path, resp.StatusCode)
		}
	}
}

func TestContexts_CRUDLifecycle(t *testing.T) {
	srv, auth := startContextServer(t)

	// Create
	body := `{"type":"tenant","id":"acme","data":{"plan":"enterprise"}}`
	status, respBody := doPost(t, srv.URL+"/v1/contexts", auth, body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/contexts: status=%d body=%s", status, respBody)
	}
	var created struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		ExternalID string          `json:"externalId"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(respBody), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" || created.Type != "tenant" || created.ExternalID != "acme" {
		t.Fatalf("unexpected created: %+v", created)
	}

	// Get
	status, respBody = doGet(t, srv.URL+"/v1/contexts/tenant/acme", auth)
	if status != http.StatusOK {
		t.Fatalf("GET: status=%d body=%s", status, respBody)
	}

	// List
	status, respBody = doGet(t, srv.URL+"/v1/contexts", auth)
	if status != http.StatusOK {
		t.Fatalf("GET list: status=%d body=%s", status, respBody)
	}

	// Update
	status, respBody = doPut(t, srv.URL+"/v1/contexts/tenant/acme", auth, `{"data":{"plan":"pro"}}`)
	if status != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", status, respBody)
	}

	// Delete
	status, _ = doDelete(t, srv.URL+"/v1/contexts/tenant/acme", auth)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d", status)
	}

	// 404 after delete
	status, _ = doGet(t, srv.URL+"/v1/contexts/tenant/acme", auth)
	if status != http.StatusNotFound {
		t.Fatalf("GET after delete: status=%d, want 404", status)
	}
}

func TestContexts_ConflictOnDuplicate(t *testing.T) {
	srv, auth := startContextServer(t)
	body := `{"type":"tenant","id":"dup"}`
	if status, _ := doPost(t, srv.URL+"/v1/contexts", auth, body); status != http.StatusCreated {
		t.Fatalf("first POST: status=%d", status)
	}
	status, respBody := doPost(t, srv.URL+"/v1/contexts", auth, body)
	if status != http.StatusConflict {
		t.Fatalf("duplicate POST: status=%d body=%s, want 409", status, respBody)
	}
}

func TestContexts_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, make([]byte, 32))

	// Two separate keys for two envs.
	key1, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap 1: %v", err)
	}
	key2, _, err := id.Bootstrap(ctx, "OtherCorp", "prod")
	if err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}

	inbox := inboxapi.New(st)
	handler := httpapi.New(id, nil, nil, inbox, nil, nil, nil, nil, nil, nil, nil, st, nil, nil, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	auth1 := "ApiKey " + key1
	auth2 := "ApiKey " + key2

	// Create in scope1.
	if status, _ := doPost(t, srv.URL+"/v1/contexts", auth1, `{"type":"tenant","id":"iso"}`); status != http.StatusCreated {
		t.Fatalf("create in scope1: status=%d", status)
	}

	// scope2 must not see it.
	status, _ := doGet(t, srv.URL+"/v1/contexts/tenant/iso", auth2)
	if status != http.StatusNotFound {
		t.Fatalf("scope isolation: status=%d, want 404", status)
	}
}
