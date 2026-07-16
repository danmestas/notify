package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
)

// startEnvVarServer boots a temp store, mints one tenant, and mounts the
// real HTTP surface with an envvars.Service wired in.
func startEnvVarServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	key, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ev, err := envvars.New(st, masterKey)
	if err != nil {
		t.Fatalf("envvars.New: %v", err)
	}
	inbox := inboxapi.New(st)
	handler := httpapi.New(id, nil, nil, inbox, nil, nil, nil, nil, nil, nil, nil, st, ev, nil, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key
}

func TestEnvVarsAPI_CreateAndListMasked(t *testing.T) {
	srv, auth := startEnvVarServer(t)

	// Create.
	status, body := doPost(t, srv.URL+"/v1/env-vars", auth, `{"key":"DB_PASS","isSecret":true,"value":"supersecret123"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/env-vars: status=%d body=%s", status, body)
	}
	if strings.Contains(body, "supersecret123") {
		t.Errorf("POST response leaked plaintext value: %s", body)
	}
	if !strings.Contains(body, envvars.ValueMaskSentinel) {
		t.Errorf("POST response missing mask sentinel: %s", body)
	}

	// List — masked.
	status, body = doGet(t, srv.URL+"/v1/env-vars", auth)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/env-vars: status=%d body=%s", status, body)
	}
	if strings.Contains(body, "supersecret123") {
		t.Errorf("GET list leaked plaintext value: %s", body)
	}
	if !strings.Contains(body, envvars.ValueMaskSentinel) {
		t.Errorf("GET list missing mask sentinel: %s", body)
	}
}

// TestEnvVarsAPI_CreateRejectsInvalidKey proves the API layer rejects an env
// var key with URL-unsafe characters (same H2-class fix as the console
// handler): the key becomes a URL path segment for the variable detail
// route, so an unvalidated key would be created but permanently unreachable.
func TestEnvVarsAPI_CreateRejectsInvalidKey(t *testing.T) {
	srv, auth := startEnvVarServer(t)

	for _, body := range []string{
		`{"key":"q2adv5 bad key!! <script>","value":"v"}`,
		`{"key":"foo/bar","value":"v"}`,
		`{"key":"a b","value":"v"}`,
		`{"key":"<script>","value":"v"}`,
	} {
		status, resp := doPost(t, srv.URL+"/v1/env-vars", auth, body)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("body=%s: status = %d, want 422 (resp: %s)", body, status, resp)
		}
	}

	// No row created despite the rejected attempts.
	status, listBody := doGet(t, srv.URL+"/v1/env-vars", auth)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/env-vars: status=%d body=%s", status, listBody)
	}
	if strings.Contains(listBody, "script") {
		t.Errorf("rejected key leaked into list: %s", listBody)
	}
}

func TestEnvVarsAPI_Reveal(t *testing.T) {
	srv, auth := startEnvVarServer(t)

	// Create.
	if status, body := doPost(t, srv.URL+"/v1/env-vars", auth, `{"key":"MY_TOKEN","isSecret":true,"value":"plaintext-value"}`); status != http.StatusCreated {
		t.Fatalf("POST: status=%d body=%s", status, body)
	}

	// Reveal=1 → plaintext.
	status, body := doGet(t, srv.URL+"/v1/env-vars?reveal=1", auth)
	if status != http.StatusOK {
		t.Fatalf("GET ?reveal=1: status=%d body=%s", status, body)
	}
	if !strings.Contains(body, "plaintext-value") {
		t.Errorf("GET ?reveal=1 missing plaintext: %s", body)
	}
}

func TestEnvVarsAPI_RevealFailClosed(t *testing.T) {
	srv, auth := startEnvVarServer(t)

	// Create.
	if status, body := doPost(t, srv.URL+"/v1/env-vars", auth, `{"key":"SAFE","isSecret":true,"value":"safe-value"}`); status != http.StatusCreated {
		t.Fatalf("POST: status=%d body=%s", status, body)
	}

	// Default list (no reveal) must mask.
	status, body := doGet(t, srv.URL+"/v1/env-vars", auth)
	if status != http.StatusOK {
		t.Fatalf("GET list: status=%d body=%s", status, body)
	}
	if strings.Contains(body, "safe-value") {
		t.Errorf("masked list leaked plaintext: %s", body)
	}
}

func TestEnvVarsAPI_Delete(t *testing.T) {
	srv, auth := startEnvVarServer(t)

	// Create.
	if status, body := doPost(t, srv.URL+"/v1/env-vars", auth, `{"key":"TO_DELETE","isSecret":false,"value":"val"}`); status != http.StatusCreated {
		t.Fatalf("POST: status=%d body=%s", status, body)
	}

	// Delete.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/env-vars/TO_DELETE", nil)
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d, want 204", resp.StatusCode)
	}

	// GET should 404.
	status, _ := doGet(t, srv.URL+"/v1/env-vars?search=TO_DELETE", auth)
	if status != http.StatusOK {
		t.Fatalf("GET after delete: status=%d", status)
	}
	// List should be empty (200 with empty items).
}

func TestEnvVarsAPI_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	key1, _, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap 1: %v", err)
	}
	key2, _, err := id.Bootstrap(ctx, "OtherCorp", "prod")
	if err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}

	ev, err := envvars.New(st, masterKey)
	if err != nil {
		t.Fatalf("envvars.New: %v", err)
	}
	inbox := inboxapi.New(st)
	handler := httpapi.New(id, nil, nil, inbox, nil, nil, nil, nil, nil, nil, nil, st, ev, nil, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	auth1 := "ApiKey " + key1
	auth2 := "ApiKey " + key2

	// Create in scope1.
	if status, body := doPost(t, srv.URL+"/v1/env-vars", auth1, `{"key":"ISOLATED","isSecret":true,"value":"s1"}`); status != http.StatusCreated {
		t.Fatalf("create scope1: status=%d body=%s", status, body)
	}

	// scope2 list must not contain scope1's key.
	status, body := doGet(t, srv.URL+"/v1/env-vars", auth2)
	if status != http.StatusOK {
		t.Fatalf("scope2 list: status=%d", status)
	}
	if strings.Contains(body, "ISOLATED") {
		t.Errorf("scope2 leaked scope1 key: %s", body)
	}
}

func TestEnvVarsAPI_AuthGuard(t *testing.T) {
	srv, _ := startEnvVarServer(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/env-vars"},
		{http.MethodPost, "/v1/env-vars"},
		{http.MethodPut, "/v1/env-vars/MY_KEY"},
		{http.MethodDelete, "/v1/env-vars/MY_KEY"},
	} {
		req, _ := http.NewRequest(route.method, srv.URL+route.path, strings.NewReader("{}"))
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
