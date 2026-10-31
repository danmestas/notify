package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/webhooksout"
)

// TestWebhook_DeliveryStatusPostedToEndpoint is the outbound-webhook slice
// proof: a real dagnats sidecar, real temp SQLite, and the real HTTP surface
// drive subscriber -> trigger -> delivered in_app message, and the
// delivery-status event fans out over the event bus to the webhooksout consumer
// which POSTs the raw message to the tenant's active webhook endpoint.
func TestWebhook_DeliveryStatusPostedToEndpoint(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	// Register the in_app delivery handler (publishing delivery-status events)
	// before workers start.
	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).HandleInApp)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	// Start the outbound webhook consumer; it subscribes to every env's
	// delivery-status events and POSTs each to that scope's active endpoints.
	// The SSRF guard blocks loopback by design; NewWithClient opts out here
	// since this test posts to a local httptest.Server (see below).
	whStop, err := webhooksout.NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second}).Start(ctx)
	if err != nil {
		t.Fatalf("webhooksout.Start: %v", err)
	}
	defer func() { _ = whStop() }()

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Customer endpoint that records received POST bodies on a buffered chan.
	received := make(chan []byte, 8)
	customer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer customer.Close()

	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL:    customer.URL,
		Active: true,
	}); err != nil {
		t.Fatalf("WebhookEndpoints.Create: %v", err)
	}

	// Author + publish a 1-step in_app workflow via the service.
	controls := json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox}))
	defer srv.Close()

	authHeader := "ApiKey " + key

	subBody := `{"subscriberId":"wh_user","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"welcome","to":{"subscriberId":"wh_user"},"payload":{"itemCount":3},"transactionId":"wh-1"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	// Within 8s the customer endpoint must receive exactly one delivery-status
	// POST whose body is the delivered in_app message.
	var raw []byte
	select {
	case raw = <-received:
	case <-time.After(8 * time.Second):
		t.Fatalf("no webhook POST arrived within 8s")
	}
	t.Logf("received webhook body: %s", raw)

	var msg domain.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode webhook body as domain.Message: %v (body=%s)", err, raw)
	}
	if msg.Channel != "in_app" {
		t.Errorf("Channel = %q, want %q", msg.Channel, "in_app")
	}
	if msg.Status != "sent" {
		t.Errorf("Status = %q, want %q", msg.Status, "sent")
	}
	if msg.SubscriberID != "wh_user" {
		t.Errorf("SubscriberID = %q, want %q", msg.SubscriberID, "wh_user")
	}
	if msg.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", msg.Subject, "Hi Ada")
	}
	if msg.TransactionID != "wh-1" {
		t.Errorf("TransactionID = %q, want %q", msg.TransactionID, "wh-1")
	}
}
