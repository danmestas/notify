package httpapi_test

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
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestInbound_FiresWorkflow proves the webhook-style ingestion path: an
// external POST to /v1/inbound/{trigger} selects the workflow by URL, carries
// the recipient + payload in the body, and drives the full
// trigger -> delivery -> in_app message pipeline over a real dagnats sidecar,
// real temp SQLite, and the real HTTP surface.
func TestInbound_FiresWorkflow(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)

	eng, err := engine.New(natsURL, bridgeURL)
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

	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).HandleInApp)
	eng.StartWorkers(ctx)

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = 0x2a
	}
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	inboundSvc := inbound.New(trg)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

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

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, inboundSvc, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	subBody := `{"subscriberId":"inb_user","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// POST /v1/inbound/welcome -> 201 + TriggerResponse. The {trigger} path
	// param selects the workflow; the body has no "name" field.
	inBody := `{"to":{"subscriberId":"inb_user"},"payload":{"itemCount":3}}`
	status, respJSON := doPost(t, srv.URL+"/v1/inbound/welcome", authHeader, inBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/inbound/welcome: status=%d body=%s", status, respJSON)
	}
	t.Logf("inbound response: %s", respJSON)

	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	if !resp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}
	if resp.Status != "processed" {
		t.Errorf("Status = %q, want %q", resp.Status, "processed")
	}

	// Poll GET /v1/messages until the delivered in_app message arrives.
	feedURL := srv.URL + "/v1/messages?subscriberId=inb_user&channel=in_app"
	var msgs []domain.Message
	var lastBody string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := doGet(t, feedURL, authHeader)
		lastBody = body
		if status != http.StatusOK {
			t.Fatalf("GET /v1/messages: status=%d body=%s", status, body)
		}
		var got []domain.Message
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode messages: %v (body=%s)", err, body)
		}
		if len(got) >= 1 {
			msgs = got
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatalf("no in_app message arrived within 10s; last feed JSON: %s", lastBody)
	}

	msg := msgs[0]
	if msg.Channel != "in_app" {
		t.Errorf("Channel = %q, want %q", msg.Channel, "in_app")
	}
	if msg.SubscriberID != "inb_user" {
		t.Errorf("SubscriberID = %q, want %q", msg.SubscriberID, "inb_user")
	}
	if msg.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", msg.Subject, "Hi Ada")
	}
	if msg.Status != "sent" {
		t.Errorf("Status = %q, want %q", msg.Status, "sent")
	}
}
