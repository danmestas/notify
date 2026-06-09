package webhooksout

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

func newBus(t *testing.T) *eventbus.Bus {
	t.Helper()
	natsURL, _ := dagtest.StartSidecar(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustJSON(t *testing.T, m domain.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestConsumer_PostsDeliveryEventToEndpoint(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	received := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    srv.URL,
		Active: true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	consumer := New(bus, st)
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{
		OrgID:   scope.OrgID,
		EnvID:   scope.EnvID,
		Channel: "in_app",
		Status:  "sent",
	}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	select {
	case body := <-received:
		var got domain.Message
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal received body: %v", err)
		}
		if got.Status != "sent" || got.OrgID != scope.OrgID || got.EnvID != scope.EnvID {
			t.Fatalf("unexpected message: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for POST")
	}

	// Ensure exactly one POST: nothing more arrives shortly after.
	select {
	case extra := <-received:
		t.Fatalf("expected exactly one POST, got extra: %s", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestConsumer_NoEndpoints_NoPost(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	consumer := New(bus, st)
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "sent"}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	select {
	case body := <-received:
		t.Fatalf("expected no POST, got: %s", body)
	case <-time.After(800 * time.Millisecond):
	}
}

func TestConsumer_OnlyActiveEndpoints(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	inactiveHit := make(chan struct{}, 1)
	inactiveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inactiveHit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer inactiveSrv.Close()

	activeHit := make(chan struct{}, 1)
	activeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeHit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer activeSrv.Close()

	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    inactiveSrv.URL,
		Active: false,
	}); err != nil {
		t.Fatalf("create inactive: %v", err)
	}
	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    activeSrv.URL,
		Active: true,
	}); err != nil {
		t.Fatalf("create active: %v", err)
	}

	consumer := New(bus, st)
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	msg := domain.Message{OrgID: scope.OrgID, EnvID: scope.EnvID, Channel: "in_app", Status: "sent"}
	if err := bus.PublishDelivery(scope.EnvID, mustJSON(t, msg)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	select {
	case <-activeHit:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for active endpoint POST")
	}

	select {
	case <-inactiveHit:
		t.Fatalf("inactive endpoint should not be POSTed to")
	case <-time.After(300 * time.Millisecond):
	}
}
