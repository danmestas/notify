package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/identity"
)

// TestTopic_AuthGuard verifies all topic management routes require auth.
func TestTopic_AuthGuard(t *testing.T) {
	srv, _, _, _, _ := startConsoleServer(t)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/topics", ""},
		{http.MethodGet, "/v1/topics/some-key", ""},
		{http.MethodPut, "/v1/topics/some-key", `{"name":"X"}`},
		{http.MethodDelete, "/v1/topics/some-key", ""},
		{http.MethodDelete, "/v1/topics/some-key/subscribers/sub-1", ""},
	}
	for _, rt := range routes {
		var status int
		switch rt.method {
		case http.MethodGet:
			status, _ = doGet(t, srv.URL+rt.path, "")
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

// TestTopic_ListEmpty returns an empty data array when no topics exist.
func TestTopic_ListEmpty(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/topics", authHeader)
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

// TestTopic_List_WithMemberCount seeds topics with members and verifies the
// member count is returned correctly in GET /v1/topics.
func TestTopic_List_WithMemberCount(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	// Create two topics: alpha with 2 members, beta with 0.
	if _, err := aud.CreateTopic(ctx, scope, "alpha", "Alpha"); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := aud.CreateTopic(ctx, scope, "beta", "Beta"); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if err := aud.AddTopicSubscriber(ctx, scope, "alpha", "sub-1"); err != nil {
		t.Fatalf("add sub-1: %v", err)
	}
	if err := aud.AddTopicSubscriber(ctx, scope, "alpha", "sub-2"); err != nil {
		t.Fatalf("add sub-2: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/topics", authHeader)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}
	var resp struct {
		Data []struct {
			Key         string `json:"key"`
			Name        string `json:"name"`
			MemberCount int    `json:"memberCount"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(resp.Data))
	}
	// Ordered by key ASC: alpha first.
	if resp.Data[0].Key != "alpha" {
		t.Errorf("data[0].key = %q, want alpha", resp.Data[0].Key)
	}
	if resp.Data[0].MemberCount != 2 {
		t.Errorf("alpha memberCount = %d, want 2", resp.Data[0].MemberCount)
	}
	if resp.Data[1].Key != "beta" {
		t.Errorf("data[1].key = %q, want beta", resp.Data[1].Key)
	}
	if resp.Data[1].MemberCount != 0 {
		t.Errorf("beta memberCount = %d, want 0", resp.Data[1].MemberCount)
	}
}

// TestTopic_GetByKey returns a topic for a known key and 404 for an unknown one.
func TestTopic_GetByKey(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	if _, err := aud.CreateTopic(ctx, scope, "vip", "VIP"); err != nil {
		t.Fatalf("create: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/topics/vip", authHeader)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}
	var got struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if got.Key != "vip" {
		t.Errorf("key = %q, want vip", got.Key)
	}
	if got.Name != "VIP" {
		t.Errorf("name = %q, want VIP", got.Name)
	}

	// Unknown key → 404.
	status, _ = doGet(t, srv.URL+"/v1/topics/ghost", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("unknown key = %d, want 404", status)
	}
}

// TestTopic_Put creates a new topic via PUT and renames an existing one.
func TestTopic_Put(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	// PUT on a non-existent key creates the topic.
	status, body := doPut(t, srv.URL+"/v1/topics/brand-new", authHeader, `{"name":"Brand New"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT create: expected 200, got %d body=%s", status, body)
	}
	var got struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if got.Key != "brand-new" || got.Name != "Brand New" {
		t.Errorf("PUT create = %+v, want key=brand-new name=Brand New", got)
	}

	// PUT on an existing key renames it.
	if _, err := aud.CreateTopic(ctx, scope, "existing", "Old Name"); err != nil {
		t.Fatalf("create existing: %v", err)
	}
	status, body = doPut(t, srv.URL+"/v1/topics/existing", authHeader, `{"name":"New Name"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT rename: expected 200, got %d body=%s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode rename: %v (body=%s)", err, body)
	}
	if got.Name != "New Name" {
		t.Errorf("renamed name = %q, want New Name", got.Name)
	}

	// Missing name → 400.
	status, _ = doPut(t, srv.URL+"/v1/topics/x", authHeader, `{}`)
	if status != http.StatusBadRequest {
		t.Errorf("missing name = %d, want 400", status)
	}
}

// TestTopic_Delete permanently removes a topic and returns 404 afterwards.
func TestTopic_Delete(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	if _, err := aud.CreateTopic(ctx, scope, "bye", "Bye"); err != nil {
		t.Fatalf("create: %v", err)
	}

	status, _ := doDelete(t, srv.URL+"/v1/topics/bye", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d", status)
	}

	// Topic is gone.
	status, _ = doGet(t, srv.URL+"/v1/topics/bye", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", status)
	}

	// Second delete → 404.
	status, _ = doDelete(t, srv.URL+"/v1/topics/bye", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", status)
	}

	// Unknown key → 404.
	status, _ = doDelete(t, srv.URL+"/v1/topics/ghost", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("delete ghost = %d, want 404", status)
	}
}

// TestTopic_DeleteSubscriber removes a member from a topic.
func TestTopic_DeleteSubscriber(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	if _, err := aud.CreateTopic(ctx, scope, "team", "Team"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := aud.AddTopicSubscriber(ctx, scope, "team", "sub-a"); err != nil {
		t.Fatalf("add sub-a: %v", err)
	}
	if err := aud.AddTopicSubscriber(ctx, scope, "team", "sub-b"); err != nil {
		t.Fatalf("add sub-b: %v", err)
	}

	// Remove sub-a.
	status, _ := doDelete(t, srv.URL+"/v1/topics/team/subscribers/sub-a", authHeader)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE member expected 204, got %d", status)
	}

	// List should now show 1 member.
	status, body := doGet(t, srv.URL+"/v1/topics", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list: %d body=%s", status, body)
	}
	var resp struct {
		Data []struct {
			Key         string `json:"key"`
			MemberCount int    `json:"memberCount"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) == 0 || resp.Data[0].MemberCount != 1 {
		t.Errorf("after remove: team memberCount = %d, want 1", resp.Data[0].MemberCount)
	}

	// Non-existent topic → 404.
	status, _ = doDelete(t, srv.URL+"/v1/topics/ghost/subscribers/sub-a", authHeader)
	if status != http.StatusNotFound {
		t.Errorf("ghost topic = %d, want 404", status)
	}
}

// TestTopic_CrossEnvIsolation verifies topics from one env are not visible from
// another.
func TestTopic_CrossEnvIsolation(t *testing.T) {
	ctx := context.Background()
	srv, authA, scopeA, _, st := startConsoleServer(t)
	aud := audience.New(st)

	if _, err := aud.CreateTopic(ctx, scopeA, "priv", "Private"); err != nil {
		t.Fatalf("create in scopeA: %v", err)
	}

	// Bootstrap scope B.
	masterKey := bytes.Repeat([]byte{0x2e}, 32)
	idB := identity.New(st, masterKey)
	keyB, _, err := idB.Bootstrap(ctx, "AcmeB", "prod")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authB := "ApiKey " + keyB

	// Scope B cannot see scope A's topic.
	status, _ := doGet(t, srv.URL+"/v1/topics/priv", authB)
	if status != http.StatusNotFound {
		t.Errorf("cross-env get = %d, want 404", status)
	}

	// Scope A can still see it.
	status, _ = doGet(t, srv.URL+"/v1/topics/priv", authA)
	if status != http.StatusOK {
		t.Errorf("scopeA get = %d, want 200", status)
	}
}

// TestTopic_AddSubscriber_EmptyID_400 pins the non-empty guard on
// POST /v1/topics/{key}/subscribers: a body that decodes to an empty or
// whitespace subscriberId (e.g. the wrong plural field name) must be a 400
// and must never insert a phantom membership row.
func TestTopic_AddSubscriber_EmptyID_400(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	if _, err := aud.CreateTopic(ctx, scope, "guard", "Guard"); err != nil {
		t.Fatalf("create topic: %v", err)
	}

	for _, body := range []string{
		`{"subscriberIds":["s1"]}`, // wrong plural field → decodes to ""
		`{"subscriberId":""}`,
		`{"subscriberId":"   "}`,
		`{}`,
	} {
		status, resp := doPost(t, srv.URL+"/v1/topics/guard/subscribers", authHeader, body)
		if status != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (resp: %s)", body, status, resp)
		}
	}

	// No phantom membership row, and memberCount stays 0.
	status, body := doGet(t, srv.URL+"/v1/topics/guard", authHeader)
	if status != http.StatusOK {
		t.Fatalf("get topic: status = %d", status)
	}
	var got struct {
		MemberCount int `json:"memberCount"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if got.MemberCount != 0 {
		t.Errorf("memberCount = %d after rejected adds, want 0", got.MemberCount)
	}
}

// TestTopic_AddSubscriber_UnknownSubscriber_422 pins the existence guard:
// memberships can never reference a subscriber that does not exist in scope.
func TestTopic_AddSubscriber_UnknownSubscriber_422(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)
	aud := audience.New(st)

	if _, err := aud.CreateTopic(ctx, scope, "exists-guard", "Exists Guard"); err != nil {
		t.Fatalf("create topic: %v", err)
	}

	status, resp := doPost(t, srv.URL+"/v1/topics/exists-guard/subscribers",
		authHeader, `{"subscriberId":"no-such-subscriber"}`)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("unknown subscriber: status = %d, want 422 (resp: %s)", status, resp)
	}

	ids, err := st.Topics().ListSubscriberIDs(ctx, scope, "exists-guard")
	if err != nil {
		t.Fatalf("list member ids: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("phantom membership rows created: %v", ids)
	}
}

// TestTopic_GetByKey_MemberCountMatchesList pins defect 5: the single-topic
// GET must carry the same memberCount that GET /v1/topics computes.
func TestTopic_GetByKey_MemberCountMatchesList(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	// Build everything over the public API: subscriber, topic, membership.
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader,
		`{"subscriberId":"mc-sub-1"}`); status != http.StatusCreated {
		t.Fatalf("create subscriber: status=%d body=%s", status, body)
	}
	if status, body := doPost(t, srv.URL+"/v1/topics", authHeader,
		`{"key":"mc-topic","name":"MC"}`); status != http.StatusCreated {
		t.Fatalf("create topic: status=%d body=%s", status, body)
	}
	if status, body := doPost(t, srv.URL+"/v1/topics/mc-topic/subscribers", authHeader,
		`{"subscriberId":"mc-sub-1"}`); status != http.StatusNoContent {
		t.Fatalf("add member: status=%d body=%s", status, body)
	}

	// Single GET.
	status, body := doGet(t, srv.URL+"/v1/topics/mc-topic", authHeader)
	if status != http.StatusOK {
		t.Fatalf("get topic: status=%d body=%s", status, body)
	}
	var single struct {
		MemberCount int `json:"memberCount"`
	}
	if err := json.Unmarshal([]byte(body), &single); err != nil {
		t.Fatalf("decode single: %v (body=%s)", err, body)
	}

	// List.
	status, body = doGet(t, srv.URL+"/v1/topics", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list topics: status=%d body=%s", status, body)
	}
	var list struct {
		Data []struct {
			Key         string `json:"key"`
			MemberCount int    `json:"memberCount"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, body)
	}
	if len(list.Data) != 1 {
		t.Fatalf("expected 1 topic in list, got %d", len(list.Data))
	}

	if single.MemberCount != 1 {
		t.Errorf("single-get memberCount = %d, want 1", single.MemberCount)
	}
	if single.MemberCount != list.Data[0].MemberCount {
		t.Errorf("single-get memberCount %d != list memberCount %d",
			single.MemberCount, list.Data[0].MemberCount)
	}
}
