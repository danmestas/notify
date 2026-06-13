package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/domain"
)

// TestLayouts_CRUD exercises the full create/read/list/update/delete cycle for
// the /v1/layouts management surface.
func TestLayouts_CRUD(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	// POST creates a new layout.
	status, body := doPost(t, srv.URL+"/v1/layouts", authHeader,
		`{"key":"welcome","name":"Welcome","content":"<h1>hi</h1>{{content}}"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST expected 201, got %d body=%s", status, body)
	}
	var created struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Name    string `json:"name"`
		Content string `json:"content"`
		OrgID   string `json:"orgId"`
		EnvID   string `json:"envId"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode create: %v (body=%s)", err, body)
	}
	if created.ID == "" {
		t.Fatalf("expected non-empty id")
	}
	if created.Key != "welcome" || created.Name != "Welcome" {
		t.Errorf("created = %+v, want key=welcome name=Welcome", created)
	}
	if created.OrgID == "" || created.EnvID == "" {
		t.Errorf("created layout missing scope: %+v", created)
	}

	// GET returns the created layout.
	status, body = doGet(t, srv.URL+"/v1/layouts/welcome", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET expected 200, got %d body=%s", status, body)
	}
	var got struct {
		Key     string `json:"key"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode get: %v (body=%s)", err, body)
	}
	if got.Key != "welcome" || got.Content != "<h1>hi</h1>{{content}}" {
		t.Errorf("got = %+v, want welcome content", got)
	}

	// GET list contains the layout.
	status, body = doGet(t, srv.URL+"/v1/layouts", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list expected 200, got %d body=%s", status, body)
	}
	var list struct {
		Layouts []struct {
			Key string `json:"key"`
		} `json:"layouts"`
		NextKey string `json:"nextKey"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, body)
	}
	if len(list.Layouts) != 1 || list.Layouts[0].Key != "welcome" {
		t.Errorf("list = %+v, want [welcome]", list.Layouts)
	}

	// PUT updates name+content (via Upsert).
	status, body = doPut(t, srv.URL+"/v1/layouts/welcome", authHeader,
		`{"name":"Welcome v2","content":"<h2>yo</h2>{{content}}"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d body=%s", status, body)
	}
	var updated struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("decode put: %v (body=%s)", err, body)
	}
	if updated.Name != "Welcome v2" || updated.Content != "<h2>yo</h2>{{content}}" {
		t.Errorf("updated = %+v, want Welcome v2", updated)
	}

	// DELETE removes the layout.
	status, _ = doDelete(t, srv.URL+"/v1/layouts/welcome", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d", status)
	}

	// GET after delete → 404.
	status, _ = doGet(t, srv.URL+"/v1/layouts/welcome", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", status)
	}
}

// TestLayouts_NotFound verifies 404 responses for unknown keys.
func TestLayouts_NotFound(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	if status, _ := doGet(t, srv.URL+"/v1/layouts/nope", authHeader); status != http.StatusNotFound {
		t.Errorf("GET unknown = %d, want 404", status)
	}
	if status, _ := doPut(t, srv.URL+"/v1/layouts/nope", authHeader, `{"name":"x","content":"{{content}}"}`); status != http.StatusOK {
		// PUT is an upsert: writing an unknown key creates it, so 200 is correct.
		t.Errorf("PUT unknown (upsert) = %d, want 200", status)
	}
	if status, _ := doDelete(t, srv.URL+"/v1/layouts/missing-key", authHeader); status != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", status)
	}
}

// TestLayouts_Conflict verifies that creating the same key twice → 409.
func TestLayouts_Conflict(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	if status, body := doPost(t, srv.URL+"/v1/layouts", authHeader,
		`{"key":"dup","name":"First","content":"{{content}}"}`); status != http.StatusCreated {
		t.Fatalf("first create = %d body=%s, want 201", status, body)
	}
	if status, _ := doPost(t, srv.URL+"/v1/layouts", authHeader,
		`{"key":"dup","name":"Second","content":"{{content}}"}`); status != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", status)
	}
}

// TestLayouts_Paging creates 5 layouts, lists with limit=3, and follows nextKey.
func TestLayouts_Paging(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if status, body := doPost(t, srv.URL+"/v1/layouts", authHeader,
			`{"key":"`+k+`","name":"N-`+k+`","content":"{{content}}"}`); status != http.StatusCreated {
			t.Fatalf("create %q = %d body=%s", k, status, body)
		}
	}

	type listResp struct {
		Layouts []struct {
			Key string `json:"key"`
		} `json:"layouts"`
		NextKey string `json:"nextKey"`
	}

	status, body := doGet(t, srv.URL+"/v1/layouts?limit=3", authHeader)
	if status != http.StatusOK {
		t.Fatalf("page1 = %d body=%s", status, body)
	}
	var page1 listResp
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if len(page1.Layouts) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1.Layouts))
	}
	if page1.NextKey == "" {
		t.Fatalf("page1 nextKey empty, want cursor")
	}

	status, body = doGet(t, srv.URL+"/v1/layouts?limit=3&after="+page1.NextKey, authHeader)
	if status != http.StatusOK {
		t.Fatalf("page2 = %d body=%s", status, body)
	}
	var page2 listResp
	if err := json.Unmarshal([]byte(body), &page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(page2.Layouts) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2.Layouts))
	}
	if page2.NextKey != "" {
		t.Errorf("page2 nextKey = %q, want empty", page2.NextKey)
	}

	seen := map[string]int{}
	for _, l := range append(page1.Layouts, page2.Layouts...) {
		seen[l.Key]++
	}
	if len(seen) != 5 {
		t.Errorf("covered %d keys across pages, want 5", len(seen))
	}
}

// TestLayouts_ScopeIsolation verifies a layout in tenant A is invisible to
// tenant B. A second tenant is minted by writing directly to the store under a
// fabricated environment scope, then asserting the API list for the real tenant
// excludes it.
func TestLayouts_ScopeIsolation(t *testing.T) {
	srv, authHeader, scope, _, st := startConsoleServer(t)

	// Seed a layout under a different environment (a foreign tenant).
	foreign := domain.Scope{OrgID: scope.OrgID, EnvID: scope.EnvID + "-other"}
	if _, err := st.Layouts().Create(context.Background(), foreign, "theirs", "Theirs", "{{content}}"); err != nil {
		t.Fatalf("seed foreign layout: %v", err)
	}
	// Seed one in the real tenant via the API.
	if status, body := doPost(t, srv.URL+"/v1/layouts", authHeader,
		`{"key":"mine","name":"Mine","content":"{{content}}"}`); status != http.StatusCreated {
		t.Fatalf("create mine = %d body=%s", status, body)
	}

	status, body := doGet(t, srv.URL+"/v1/layouts", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list = %d body=%s", status, body)
	}
	var list struct {
		Layouts []struct {
			Key string `json:"key"`
		} `json:"layouts"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Layouts) != 1 || list.Layouts[0].Key != "mine" {
		t.Errorf("tenant list = %+v, want [mine] only (no cross-tenant leak)", list.Layouts)
	}

	// And a direct GET of the foreign key is a 404 for this tenant.
	if status, _ := doGet(t, srv.URL+"/v1/layouts/theirs", authHeader); status != http.StatusNotFound {
		t.Errorf("GET foreign key = %d, want 404", status)
	}
}
