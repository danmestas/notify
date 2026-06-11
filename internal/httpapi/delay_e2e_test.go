package httpapi_test

import (
	"bytes"
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
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestDelay_InAppDeliveredAfterSleep proves the DELAY slice end-to-end: a
// two-step workflow [delay(1s) -> in_app] delivers the in_app message only
// AFTER the delay elapses, and the post-delay step self-sources its render
// context from the store (its TaskPayload.Input is empty because the upstream
// sleep step produces no output).
func TestDelay_InAppDeliveredAfterSleep(t *testing.T) {
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

	// Two-step workflow: delay 1s, then an in_app channel step.
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Delayed",
		TriggerIdentifier: "delayed",
		Steps: []domain.Step{
			{
				StepID:        "d1",
				Type:          "delay",
				ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`),
			},
			{
				StepID:        "S1",
				Type:          "in_app",
				ControlValues: json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`),
			},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "delayed"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"d_user","firstName":"Ada"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	start := time.Now()
	trigBody := `{"name":"delayed","to":{"subscriberId":"d_user"},"payload":{"itemCount":3},"transactionId":"d-1"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	feedURL := srv.URL + "/v1/messages?subscriberId=d_user&channel=in_app"
	sentCount := func(body string) int {
		var got []domain.Message
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode messages: %v (body=%s)", err, body)
		}
		n := 0
		for _, m := range got {
			if m.Status == "sent" {
				n++
			}
		}
		return n
	}

	// Early check (~300ms): still sleeping, so no sent in_app message yet.
	time.Sleep(300 * time.Millisecond)
	status, earlyBody := doGet(t, feedURL, authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/messages (early): status=%d body=%s", status, earlyBody)
	}
	if n := sentCount(earlyBody); n != 0 {
		// Not fatal: if the environment is unusually slow this is racy, but the
		// 1s sleep + ~5s poll latency make an early send extremely unlikely.
		t.Errorf("delay not observed: %d sent in_app messages at %s (want 0); body=%s", n, time.Since(start), earlyBody)
	} else {
		t.Logf("delay observed: 0 sent in_app messages at %s (still sleeping)", time.Since(start))
	}

	// Eventual delivery: the in_app message arrives after the delay, rendered
	// from self-sourced context.
	var msgs []domain.Message
	var lastBody string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := doGet(t, feedURL, authHeader)
		lastBody = body
		if status != http.StatusOK {
			t.Fatalf("GET /v1/messages: status=%d body=%s", status, body)
		}
		if sentCount(body) >= 1 {
			if err := json.Unmarshal([]byte(body), &msgs); err != nil {
				t.Fatalf("decode messages: %v (body=%s)", err, body)
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	elapsed := time.Since(start)
	if sentCount(lastBody) < 1 {
		t.Fatalf("no sent in_app message arrived within 10s; last feed JSON: %s", lastBody)
	}
	t.Logf("in_app message delivered after delay in %s", elapsed)

	var msg domain.Message
	for _, m := range msgs {
		if m.Status == "sent" {
			msg = m
			break
		}
	}
	if msg.Subject != "Hi Ada" {
		t.Errorf("Subject = %q, want %q", msg.Subject, "Hi Ada")
	}
	if msg.Content != "You have 3 items" {
		t.Errorf("Content = %q, want %q", msg.Content, "You have 3 items")
	}
	if msg.Status != "sent" {
		t.Errorf("Status = %q, want %q", msg.Status, "sent")
	}
	if msg.TransactionID != "d-1" {
		t.Errorf("TransactionID = %q, want %q", msg.TransactionID, "d-1")
	}
	if elapsed < 1*time.Second {
		t.Errorf("delivered in %s, want >= 1s (the delay)", elapsed)
	}
}
