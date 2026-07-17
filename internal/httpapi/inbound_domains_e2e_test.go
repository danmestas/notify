package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// startInboundDomainsServer boots an httpapi server with inbound domains wired.
func startInboundDomainsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := t.TempDir() + "/notify.db"
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	masterKey := bytes.Repeat([]byte{0xab}, 32)
	id := identity.New(st, masterKey)
	key, _, err := id.Bootstrap(ctx, "TestOrg", "TestEnv")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	h := httpapi.New(httpapi.Deps{Identity: id, Store: st})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key
}

func TestInboundDomains_CRUD(t *testing.T) {
	srv, authHeader := startInboundDomainsServer(t)

	// Create.
	status, body := doPost(t, srv.URL+"/v1/inbound/domains", authHeader, `{"name":"test.com"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST: %d body=%s", status, body)
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.Name != "test.com" {
		t.Errorf("Name = %q, want test.com", created.Name)
	}
	if created.Status != "pending" {
		t.Errorf("Status = %q, want pending", created.Status)
	}

	// List.
	status, listBody := doGet(t, srv.URL+"/v1/inbound/domains", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET list: %d", status)
	}
	var listResp struct {
		Data       []any `json:"data"`
		TotalCount int   `json:"totalCount"`
	}
	if err := json.Unmarshal([]byte(listBody), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listResp.TotalCount != 1 {
		t.Errorf("totalCount = %d, want 1", listResp.TotalCount)
	}

	// Get by name.
	status, getBody := doGet(t, srv.URL+"/v1/inbound/domains/test.com", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET: %d body=%s", status, getBody)
	}

	// Verify (will fail DNS for test.com — expected status=failed, not 500).
	status, verifyBody := doPost(t, srv.URL+"/v1/inbound/domains/test.com/verify", authHeader, ``)
	if status != http.StatusOK {
		t.Fatalf("verify: %d body=%s", status, verifyBody)
	}
	var verified struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(verifyBody), &verified); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	// test.com probably has real MX records but NOT pointing to inbound.notify.example.com
	// so it should be failed (or possibly verified if DNS resolves to our host — but never 500).
	if verified.Status != "failed" && verified.Status != "verified" {
		t.Errorf("Status after verify = %q, want failed or verified", verified.Status)
	}

	// Delete.
	status, _ = doDelete(t, srv.URL+"/v1/inbound/domains/test.com", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE: %d", status)
	}

	// Get after delete → 404.
	status, _ = doGet(t, srv.URL+"/v1/inbound/domains/test.com", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", status)
	}
}

func TestInboundDomains_404(t *testing.T) {
	srv, authHeader := startInboundDomainsServer(t)
	status, _ := doGet(t, srv.URL+"/v1/inbound/domains/nonexistent.com", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET nonexistent = %d, want 404", status)
	}
}

func TestInboundDomains_Paging(t *testing.T) {
	srv, authHeader := startInboundDomainsServer(t)
	for _, n := range []string{"alpha.com", "beta.com", "gamma.com"} {
		if s, b := doPost(t, srv.URL+"/v1/inbound/domains", authHeader, `{"name":"`+n+`"}`); s != http.StatusCreated {
			t.Fatalf("create %s: %d body=%s", n, s, b)
		}
	}
	s, body := doGet(t, srv.URL+"/v1/inbound/domains?limit=2", authHeader)
	if s != http.StatusOK {
		t.Fatalf("list: %d", s)
	}
	var resp struct {
		Data []struct{ Name string } `json:"data"`
		Next string                  `json:"next"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Errorf("page1 len = %d, want 2", len(resp.Data))
	}
}

func TestInboundDomains_Unauthenticated(t *testing.T) {
	srv, _ := startInboundDomainsServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/inbound/domains", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", resp.StatusCode)
	}
}
