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

	"github.com/danmestas/notify/internal/analytics"
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

// TestAnalytics_DeliveriesByChannel is the analytics-slice (wave B) proof: a
// real dagnats sidecar, real temp SQLite, and the real HTTP surface drive
// subscriber -> trigger (twice) -> delivered in_app messages. Each in_app send
// publishes a delivery-status event that the analytics projector drains
// asynchronously into the analytics store. Polling GET /v1/analytics/deliveries
// must observe in_app.sent >= 2.
func TestAnalytics_DeliveriesByChannel(t *testing.T) {
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

	// in_app delivery handler publishes delivery-status events on the bus.
	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).HandleInApp)
	eng.StartWorkers(ctx)

	// Projector drains every env's delivery-status events into analytics.
	an := analytics.New(st, bus)
	anStop, err := an.Start(ctx)
	if err != nil {
		t.Fatalf("analytics.Start: %v", err)
	}
	defer func() { _ = anStop() }()

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

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, an, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	subBody := `{"subscriberId":"a_user","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	for _, tx := range []string{"a1", "a2"} {
		trigBody := `{"name":"welcome","to":{"subscriberId":"a_user"},"payload":{"itemCount":3},"transactionId":"` + tx + `"}`
		if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger %s: status=%d body=%s", tx, status, body)
		}
	}

	// Poll the analytics query API until the projector has ingested both sends.
	feedURL := srv.URL + "/v1/analytics/deliveries"
	var got map[string]map[string]int
	var lastBody string
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		status, body := doGet(t, feedURL, authHeader)
		lastBody = body
		if status != http.StatusOK {
			t.Fatalf("GET /v1/analytics/deliveries: status=%d body=%s", status, body)
		}
		var m map[string]map[string]int
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("decode analytics: %v (body=%s)", err, body)
		}
		if m["in_app"]["sent"] >= 2 {
			got = m
			break
		}
		t.Logf("poll: in_app.sent=%d (body=%s)", m["in_app"]["sent"], body)
		time.Sleep(300 * time.Millisecond)
	}
	if got == nil {
		t.Fatalf("in_app.sent never reached 2 within 25s; last body: %s", lastBody)
	}
	t.Logf("analytics deliveries: %v", got)
	if got["in_app"]["sent"] < 2 {
		t.Errorf("in_app.sent = %d, want >= 2", got["in_app"]["sent"])
	}

	// The over-time series must include today's in_app/sent bucket (>= 2 sends).
	tsStatus, tsBody := doGet(t, srv.URL+"/v1/analytics/timeseries", authHeader)
	if tsStatus != http.StatusOK {
		t.Fatalf("GET /v1/analytics/timeseries: status=%d body=%s", tsStatus, tsBody)
	}
	var series []domain.DeliveryDayCount
	if err := json.Unmarshal([]byte(tsBody), &series); err != nil {
		t.Fatalf("decode timeseries: %v (body=%s)", err, tsBody)
	}
	var tsInAppSent int
	for _, b := range series {
		if b.Channel == "in_app" && b.Status == "sent" {
			tsInAppSent += b.Count
			if len(b.Day) != 10 {
				t.Errorf("timeseries day not YYYY-MM-DD: %q", b.Day)
			}
		}
	}
	if tsInAppSent < 2 {
		t.Errorf("timeseries in_app/sent total = %d, want >= 2 (body=%s)", tsInAppSent, tsBody)
	}
	t.Logf("timeseries: %s", tsBody)
}
