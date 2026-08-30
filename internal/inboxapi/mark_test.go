package inboxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
)

// doPost issues an authenticated POST through the inbox routes and returns
// the recorder, mirroring doGet in inboxapi_test.go.
func doPost(t *testing.T, svc *Service, target string, scope *domain.Scope, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	svc.Routes(r)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if scope != nil {
		req = req.WithContext(domain.ContextWithScope(req.Context(), *scope))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestMarkMessage_Seen(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/seen", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	got, err := s.Messages().Get(context.Background(), scope, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Seen {
		t.Error("expected Seen=true after POST .../seen")
	}
}

func TestMarkMessage_ReadThenUnread(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)

	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/read", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("read: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got, _ := s.Messages().Get(context.Background(), scope, m.ID)
	if !got.Read {
		t.Fatal("expected Read=true after POST .../read")
	}

	rec = doPost(t, svc, "/v1/messages/"+m.ID+"/unread", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unread: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got, _ = s.Messages().Get(context.Background(), scope, m.ID)
	if got.Read {
		t.Error("expected Read=false after POST .../unread")
	}
}

func TestMarkMessage_ArchiveThenUnarchive(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)

	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/archive", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("archive: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got, _ := s.Messages().Get(context.Background(), scope, m.ID)
	if !got.Archived {
		t.Fatal("expected Archived=true after POST .../archive")
	}

	rec = doPost(t, svc, "/v1/messages/"+m.ID+"/unarchive", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unarchive: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got, _ = s.Messages().Get(context.Background(), scope, m.ID)
	if got.Archived {
		t.Error("expected Archived=false after POST .../unarchive")
	}
}

func TestMarkMessage_UnknownAction(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/bogus", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarkMessage_MissingSubscriberID(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/seen", &scope, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarkMessage_WrongSubscriber_404 verifies that a mismatched
// subscriberId is rejected as 404 — not 403 — so the response gives no
// signal distinguishing "wrong subscriber" from "no such message".
func TestMarkMessage_WrongSubscriber_404(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/seen", &scope, `{"subscriberId":"someone-else"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := s.Messages().Get(context.Background(), scope, m.ID)
	if got.Seen {
		t.Error("message must not be marked seen when subscriberId mismatches")
	}
}

// TestMarkMessage_OtherOrg_404 verifies an id from a different org/env
// (identified purely by the API key's scope) is also a 404, not a leak.
func TestMarkMessage_OtherOrg_404(t *testing.T) {
	s := newTestStore(t)
	ownerScope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	otherScope := domain.Scope{OrgID: "org2", EnvID: "envB"}
	m, err := s.Messages().Create(context.Background(), ownerScope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m.ID+"/seen", &otherScope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	got, err := s.Messages().Get(context.Background(), ownerScope, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Seen {
		t.Error("cross-org mark attempt must not flip Seen on the owning org's message")
	}
}

// TestMarkMessagesBulk_OtherOrg_NoOp verifies a bulk mark scoped to a
// different org/env than the message's own updates nothing — mirroring
// TestMarkMessage_OtherOrg_404's single-message guarantee for the bulk
// path, where cross-org rows are silently excluded rather than a 404.
func TestMarkMessagesBulk_OtherOrg_NoOp(t *testing.T) {
	s := newTestStore(t)
	ownerScope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	otherScope := domain.Scope{OrgID: "org2", EnvID: "envB"}
	m, err := s.Messages().Create(context.Background(), ownerScope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	body := `{"subscriberId":"u1","ids":["` + m.ID + `"],"as":"seen"}`
	rec := doPost(t, svc, "/v1/messages/mark", &otherScope, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Updated != 0 {
		t.Errorf("updated = %d, want 0 (cross-org bulk mark must be a no-op)", resp.Updated)
	}

	got, err := s.Messages().Get(context.Background(), ownerScope, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Seen {
		t.Error("cross-org bulk mark must not flip Seen on the owning org's message")
	}
}

func TestMarkMessagesBulk_ByIDs(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m1, _ := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})
	m2, _ := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})
	m3, _ := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})

	svc := New(s)
	body := `{"subscriberId":"u1","ids":["` + m1.ID + `","` + m2.ID + `"],"as":"read"}`
	rec := doPost(t, svc, "/v1/messages/mark", &scope, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if resp.Updated != 2 {
		t.Errorf("updated = %d, want 2", resp.Updated)
	}

	got1, _ := s.Messages().Get(context.Background(), scope, m1.ID)
	got2, _ := s.Messages().Get(context.Background(), scope, m2.ID)
	got3, _ := s.Messages().Get(context.Background(), scope, m3.ID)
	if !got1.Read || !got2.Read {
		t.Error("expected m1 and m2 to be marked read")
	}
	if got3.Read {
		t.Error("m3 was not in the ids list and must be untouched")
	}
}

func TestMarkMessagesBulk_All(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	for i := 0; i < 3; i++ {
		if _, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/mark", &scope, `{"subscriberId":"u1","all":true,"as":"archived"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Updated != 3 {
		t.Errorf("updated = %d, want 3", resp.Updated)
	}
}

// TestMarkMessagesBulk_OtherSubscriberIDsSkipped verifies that an id list
// mixing in another subscriber's message id silently excludes that row
// rather than marking it (or erroring in a way that would confirm its
// existence).
func TestMarkMessagesBulk_OtherSubscriberIDsSkipped(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	mine, _ := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})
	theirs, _ := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u2", Channel: "in_app", Status: "sent"})

	svc := New(s)
	body := `{"subscriberId":"u1","ids":["` + mine.ID + `","` + theirs.ID + `"],"as":"seen"}`
	rec := doPost(t, svc, "/v1/messages/mark", &scope, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Updated != 1 {
		t.Errorf("updated = %d, want 1 (only the caller's own message)", resp.Updated)
	}

	gotTheirs, _ := s.Messages().Get(context.Background(), scope, theirs.ID)
	if gotTheirs.Seen {
		t.Error("another subscriber's message must not be marked seen")
	}
}

func TestMarkMessagesBulk_UnknownAs(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/mark", &scope, `{"subscriberId":"u1","all":true,"as":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarkMessagesBulk_MissingIDsAndAll(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/mark", &scope, `{"subscriberId":"u1","as":"read"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
