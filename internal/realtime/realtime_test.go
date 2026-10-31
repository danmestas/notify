package realtime

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
)

// scopeInjector mimics identity.Middleware by stamping a fixed Scope onto the
// request context so the gateway sees a tenant.
func scopeInjector(s domain.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(domain.ContextWithScope(r.Context(), s)))
	})
}

func TestGateway_StreamsPublishedInboxEvents(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = bus.Close() }()

	g := New(bus)
	r := chi.NewRouter()
	g.Routes(r)
	handler := scopeInjector(domain.Scope{OrgID: "org1", EnvID: "env1"}, r)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		srv.URL+"/v1/inbox/stream?subscriberId=sub1", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				got <- line
				return
			}
		}
	}()

	// Publish after the subscription is live; retry until the stream picks it
	// up to avoid racing the SubscribeInbox setup.
	want := []byte(`{"id":"m1"}`)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(3 * time.Second)

	if err := bus.PublishInbox("env1", "sub1", want); err != nil {
		t.Fatalf("PublishInbox: %v", err)
	}
	for {
		select {
		case line := <-got:
			if !strings.Contains(line, `{"id":"m1"}`) {
				t.Fatalf("data line = %q, want it to contain the payload", line)
			}
			reqCancel() // disconnect
			return
		case <-ticker.C:
			if err := bus.PublishInbox("env1", "sub1", want); err != nil {
				t.Fatalf("PublishInbox: %v", err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for SSE data frame")
		}
	}
}

func TestGateway_NoScope_401(t *testing.T) {
	g := New(nil)
	r := chi.NewRouter()
	g.Routes(r)

	req := httptest.NewRequest(http.MethodGet, "/v1/inbox/stream?subscriberId=sub1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGateway_NoSubscriberId_400(t *testing.T) {
	g := New(nil)
	r := chi.NewRouter()
	g.Routes(r)
	handler := scopeInjector(domain.Scope{OrgID: "org1", EnvID: "env1"}, r)

	req := httptest.NewRequest(http.MethodGet, "/v1/inbox/stream", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
