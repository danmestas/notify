package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestRegisterDeliveryHandlers_EmailPublishesDeliveryEvent guards against a
// repeat of the wiring bug where send.email's registration in runServe
// silently omitted .WithEventBus (notify hardening #8): every channel is now
// wired through wireCommon, whose common[T] constraint cannot omit one of the
// four shared dependencies without a compile error. This exercises the real
// registerDeliveryHandlers wiring end-to-end — a real dagnats sidecar, a real
// NATS-backed event bus, and a real SQLite store — and asserts an actual
// delivery-status event reaches a bus subscriber once send.email completes,
// not just that EmailHandler accepts a publisher (that part was already
// covered by internal/delivery's own unit tests).
func TestRegisterDeliveryHandlers_EmailPublishesDeliveryEvent(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = 0x42
	}
	conn := connectivity.New(st, mk)
	prefs := preferences.New(st)

	registerDeliveryHandlers(eng, st, conn, prefs, bus)
	eng.StartWorkers(ctx)

	scope := domain.Scope{OrgID: "acme", EnvID: "wiring-test"}

	// Stand-in ESP: any 2xx response is a successful send. No mocks: this is
	// the real webhook.EmailClient posting over real HTTP.
	esp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(esp.Close)
	if _, err := conn.CreateEmailWebhook(ctx, scope, "email-wh", connectivity.EmailWebhookConfig{WebhookURL: esp.URL}, true); err != nil {
		t.Fatalf("CreateEmailWebhook: %v", err)
	}

	aud := audience.New(st)
	if _, err := aud.Upsert(ctx, scope, audience.SubscriberInput{SubscriberID: "sub1", Email: "sub1@example.com"}); err != nil {
		t.Fatalf("Upsert subscriber: %v", err)
	}

	au := authoring.New(st, eng)
	controls := json.RawMessage(`{"subject":"Hi","body":"hello"}`)
	if _, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Wiring Email",
		TriggerIdentifier: "wiring-email",
		Steps:             []domain.Step{{StepID: "E1", Type: "email", ControlValues: controls}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := au.Publish(ctx, scope, "wiring-email"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	// Subscribe before triggering so the publish can't race ahead of the
	// subscription.
	received := make(chan []byte, 1)
	unsub, err := bus.SubscribeDelivery(scope.EnvID, func(data []byte) {
		select {
		case received <- data:
		default:
		}
	})
	if err != nil {
		t.Fatalf("SubscribeDelivery: %v", err)
	}
	t.Cleanup(func() { _ = unsub() })

	trg := trigger.New(aud, st, eng)
	resp, err := trg.Ingest(ctx, scope, trigger.TriggerRequest{
		Name:          "wiring-email",
		To:            trigger.ToField{SubscriberID: "sub1"},
		TransactionID: "wire-1",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !resp.Acknowledged {
		t.Fatalf("Ingest not acknowledged: %+v", resp)
	}

	select {
	case data := <-received:
		var msg domain.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal delivery-status event: %v (data=%s)", err, data)
		}
		if msg.Channel != "email" {
			t.Errorf("delivery event Channel = %q, want %q", msg.Channel, "email")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no delivery-status event published for send.email within 20s — registerDeliveryHandlers must wire send.email through wireCommon (WithEventBus)")
	}
}
