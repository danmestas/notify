package inboxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func doGet(t *testing.T, svc *Service, target string, scope *domain.Scope) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	svc.Routes(r)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if scope != nil {
		req = req.WithContext(domain.ContextWithScope(req.Context(), *scope))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestListMessages_ReturnsSeeded(t *testing.T) {
	s := newTestStore(t)
	scopeA := domain.Scope{OrgID: "org1", EnvID: "envA"}

	seeded, err := s.Messages().Create(context.Background(), scopeA, domain.Message{
		SubscriberID: "u1",
		Channel:      "in_app",
		Content:      "hello world",
		Subject:      "Greetings",
		Status:       "sent",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app&format=array", &scopeA)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []domain.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0]
	if m.ID != seeded.ID {
		t.Errorf("id = %q, want %q", m.ID, seeded.ID)
	}
	if m.Content != "hello world" {
		t.Errorf("content = %q", m.Content)
	}
	if m.Subject != "Greetings" {
		t.Errorf("subject = %q", m.Subject)
	}
	if m.Channel != "in_app" {
		t.Errorf("channel = %q", m.Channel)
	}
	if m.Status != "sent" {
		t.Errorf("status = %q", m.Status)
	}
}

func TestListMessages_ScopeIsolation(t *testing.T) {
	s := newTestStore(t)
	scopeA := domain.Scope{OrgID: "org1", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "org1", EnvID: "envB"}

	if _, err := s.Messages().Create(context.Background(), scopeA, domain.Message{
		SubscriberID: "u1",
		Channel:      "in_app",
		Content:      "secret",
		Status:       "sent",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app&format=array", &scopeB)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []domain.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0 (scope isolation breached)", len(got))
	}
}

func TestListMessages_NoScope_Unauthorized(t *testing.T) {
	s := newTestStore(t)
	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app&format=array", nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestAcceptsCompatArray covers the Accept-header media-type parsing used
// to opt into the legacy bare-array response: it must match on media type
// alone, ignoring parameters (e.g. ";q=1") and tolerating the type
// appearing anywhere within a comma-separated Accept list.
func TestAcceptsCompatArray(t *testing.T) {
	cases := []struct {
		name   string
		accept string
		want   bool
	}{
		{"exact", "application/vnd.notify.v1+json", true},
		{"with q-value", "application/vnd.notify.v1+json;q=1", true},
		{"with q-value and space", "application/vnd.notify.v1+json; q=0.9", true},
		{"in a list, not first", "text/html, application/vnd.notify.v1+json;q=0.8", true},
		{"in a list, first with params", "application/vnd.notify.v1+json;q=1, text/html", true},
		{"unrelated type", "application/json", false},
		{"empty", "", false},
		{"prefix collision", "application/vnd.notify.v1+jsonx", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptsCompatArray(tc.accept); got != tc.want {
				t.Errorf("acceptsCompatArray(%q) = %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}

// TestListMessages_AcceptHeader_QValue_OptsIntoArray verifies the HTTP path
// end-to-end: an Accept header carrying a q-value still selects the legacy
// bare-array shape, not the envelope.
func TestListMessages_AcceptHeader_QValue_OptsIntoArray(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	r := chi.NewRouter()
	svc.Routes(r)
	req := httptest.NewRequest(http.MethodGet, "/v1/messages?subscriberId=u1&channel=in_app", nil)
	req.Header.Set("Accept", "application/vnd.notify.v1+json;q=1")
	req = req.WithContext(domain.ContextWithScope(req.Context(), scope))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []domain.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("expected a bare array (legacy shape) for Accept with q-value; unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}
