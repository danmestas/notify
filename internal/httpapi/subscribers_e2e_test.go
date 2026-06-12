package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/identity"
)

// TestSubscriber_AuthGuard verifies all subscriber management routes require auth.
func TestSubscriber_AuthGuard(t *testing.T) {
	srv, _, _, _, _ := startConsoleServer(t)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/subscribers", ""},
		{http.MethodGet, "/v1/subscribers/some-id", ""},
		{http.MethodPatch, "/v1/subscribers/some-id", `{}`},
		{http.MethodDelete, "/v1/subscribers/some-id", ""},
	}
	for _, rt := range routes {
		var status int
		switch rt.method {
		case http.MethodGet:
			status, _ = doGet(t, srv.URL+rt.path, "")
		case http.MethodPatch:
			status, _ = doPatch(t, srv.URL+rt.path, "", rt.body)
		case http.MethodDelete:
			status, _ = doDelete(t, srv.URL+rt.path, "")
		}
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: status=%d, want 401", rt.method, rt.path, status)
		}
	}
}

// TestSubscriber_ListEmpty returns an empty data array when there are no
// non-deleted subscribers in scope.
func TestSubscriber_ListEmpty(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/subscribers", authHeader)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}
	var resp struct {
		Data       []any `json:"data"`
		TotalCount int   `json:"totalCount"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(resp.Data) != 0 || resp.TotalCount != 0 {
		t.Errorf("expected empty list, got %+v", resp)
	}
}

// TestSubscriber_List_CursorPagination seeds several subscribers and walks
// through cursor pages, verifying no duplicates and complete coverage.
func TestSubscriber_List_CursorPagination(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	// Seed 5 subscribers.
	for i := range 5 {
		if _, err := aud.Upsert(ctx, scope, audience.SubscriberInput{
			SubscriberID: fmt.Sprintf("sub-%d", i),
			Email:        fmt.Sprintf("s%d@x.com", i),
		}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	// Collect all pages with limit=2.
	var allIDs []string
	cursor := ""
	for {
		url := srv.URL + "/v1/subscribers?limit=2"
		if cursor != "" {
			url += "&after=" + cursor
		}
		status, body := doGet(t, url, authHeader)
		if status != http.StatusOK {
			t.Fatalf("list page: status=%d body=%s", status, body)
		}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Next       string `json:"next"`
			TotalCount int    `json:"totalCount"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
		if resp.TotalCount != 5 {
			t.Errorf("totalCount = %d, want 5", resp.TotalCount)
		}
		for _, d := range resp.Data {
			allIDs = append(allIDs, d.ID)
		}
		if resp.Next == "" {
			break
		}
		cursor = resp.Next
	}

	if len(allIDs) != 5 {
		t.Fatalf("collected %d subscribers across pages, want 5", len(allIDs))
	}
	seen := map[string]bool{}
	for _, id := range allIDs {
		if seen[id] {
			t.Errorf("duplicate id %q across pages", id)
		}
		seen[id] = true
	}
}

// TestSubscriber_GetByID returns the subscriber for a known id and 404 for an
// unknown one.
func TestSubscriber_GetByID(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	sub, err := aud.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "s1",
		Email:        "s1@x.com",
		FirstName:    "Ada",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/subscribers/"+sub.ID, authHeader)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}
	var got struct {
		ID           string `json:"id"`
		SubscriberID string `json:"subscriberId"`
		FirstName    string `json:"firstName"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if got.ID != sub.ID {
		t.Errorf("id = %q, want %q", got.ID, sub.ID)
	}
	if got.SubscriberID != "s1" {
		t.Errorf("subscriberId = %q, want s1", got.SubscriberID)
	}
	if got.FirstName != "Ada" {
		t.Errorf("firstName = %q, want Ada", got.FirstName)
	}

	// Unknown id → 404.
	status, _ = doGet(t, srv.URL+"/v1/subscribers/nonexistent", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for unknown id, got %d", status)
	}
}

// TestSubscriber_Patch updates mutable profile fields.
func TestSubscriber_Patch(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	sub, err := aud.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "s1",
		Email:        "old@x.com",
		FirstName:    "Old",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	patchBody := `{"email":"new@x.com","firstName":"New","phone":"+15555550100"}`
	status, respBody := doPatch(t, srv.URL+"/v1/subscribers/"+sub.ID, authHeader, patchBody)
	if status != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d body=%s", status, respBody)
	}
	var got struct {
		Email     string `json:"email"`
		FirstName string `json:"firstName"`
		Phone     string `json:"phone"`
	}
	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, respBody)
	}
	if got.Email != "new@x.com" {
		t.Errorf("email = %q, want new@x.com", got.Email)
	}
	if got.FirstName != "New" {
		t.Errorf("firstName = %q, want New", got.FirstName)
	}
	if got.Phone != "+15555550100" {
		t.Errorf("phone = %q, want +15555550100", got.Phone)
	}

	// Unknown id → 404.
	status, _ = doPatch(t, srv.URL+"/v1/subscribers/nope", authHeader, `{"email":"x@x.com"}`)
	if status != http.StatusNotFound {
		t.Errorf("PATCH unknown = %d, want 404", status)
	}
}

// TestSubscriber_Delete soft-deletes a subscriber and verifies it disappears
// from subsequent List calls.
func TestSubscriber_Delete(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	sub, err := aud.Upsert(ctx, scope, audience.SubscriberInput{
		SubscriberID: "to-del",
		Email:        "del@x.com",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Delete returns 204.
	status, _ := doDelete(t, srv.URL+"/v1/subscribers/"+sub.ID, authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d", status)
	}

	// Deleted subscriber absent from list.
	status, body := doGet(t, srv.URL+"/v1/subscribers", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list after delete: %d body=%s", status, body)
	}
	var resp struct {
		Data       []any `json:"data"`
		TotalCount int   `json:"totalCount"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 0 || resp.TotalCount != 0 {
		t.Errorf("deleted subscriber still in list: %+v", resp)
	}

	// Second delete → 404.
	status, _ = doDelete(t, srv.URL+"/v1/subscribers/"+sub.ID, authHeader)
	if status != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", status)
	}

	// Unknown id → 404.
	status, _ = doDelete(t, srv.URL+"/v1/subscribers/nope", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("delete unknown = %d, want 404", status)
	}
}

// TestSubscriber_CrossEnvIsolation verifies that subscribers from one env are
// not visible from another env's scope.
func TestSubscriber_CrossEnvIsolation(t *testing.T) {
	ctx := context.Background()
	srv, authA, scopeA, _, st := startConsoleServer(t)
	aud := audience.New(st)

	sub, err := aud.Upsert(ctx, scopeA, audience.SubscriberInput{
		SubscriberID: "shared",
		Email:        "a@x.com",
	})
	if err != nil {
		t.Fatalf("upsert in scopeA: %v", err)
	}

	// Bootstrap scope B.
	masterKey := bytes.Repeat([]byte{0x2d}, 32)
	idB := identity.New(st, masterKey)
	keyB, _, err := idB.Bootstrap(ctx, "AcmeB", "prod")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authB := "ApiKey " + keyB

	// Scope B cannot get scope A's subscriber.
	status, _ := doGet(t, srv.URL+"/v1/subscribers/"+sub.ID, authB)
	if status != http.StatusNotFound {
		t.Errorf("cross-env get = %d, want 404", status)
	}

	// Scope A's list returns 1, scope B's list returns 0.
	_, bodyA := doGet(t, srv.URL+"/v1/subscribers", authA)
	var respA struct {
		TotalCount int `json:"totalCount"`
	}
	_ = json.Unmarshal([]byte(bodyA), &respA)
	if respA.TotalCount != 1 {
		t.Errorf("scopeA totalCount = %d, want 1", respA.TotalCount)
	}

	_, bodyB := doGet(t, srv.URL+"/v1/subscribers", authB)
	var respB struct {
		TotalCount int `json:"totalCount"`
	}
	_ = json.Unmarshal([]byte(bodyB), &respB)
	if respB.TotalCount != 0 {
		t.Errorf("scopeB totalCount = %d, want 0", respB.TotalCount)
	}
}
