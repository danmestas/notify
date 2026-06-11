package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
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

// TestThrottle_SecondSendThrottled is the per-step rate-limit proof: a real
// dagnats sidecar, real temp SQLite, and the real HTTP surface drive two
// triggers for the same subscriber against an in_app workflow whose step
// configures throttle {limit:1,windowSec:60}. Exactly one message is "sent"
// and the second is "throttled".
func TestThrottle_SecondSendThrottled(t *testing.T) {
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

	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).WithThrottle(bus).HandleInApp)
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

	// windowSec is large so both sends fall in the same epoch-aligned fixed
	// window; the eventbus counter buckets by UnixNano/window, so a 60s window
	// is flaky when the two sends straddle a wall-clock minute boundary. The
	// behavior under test is limit=1 -> exactly one send, not the window value.
	controls := json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"hello","throttle":{"limit":1,"windowSec":3600}}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome-flow",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "welcome-flow"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	subBody := `{"subscriberId":"user_123","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	feedURL := srv.URL + "/v1/messages?subscriberId=user_123&channel=in_app"
	pollFor := func(want int) []domain.Message {
		t.Helper()
		var lastBody string
		deadline := time.Now().Add(20 * time.Second)
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
			if len(got) >= want {
				return got
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("expected %d in_app messages within 20s; last feed JSON: %s", want, lastBody)
		return nil
	}
	trigger1 := func(tx string) {
		t.Helper()
		trigBody := `{"name":"welcome-flow","to":{"subscriberId":"user_123"},"payload":{},"transactionId":"` + tx + `"}`
		if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger %s: status=%d body=%s", tx, status, body)
		}
	}

	// Fire t1 and wait for its message to land BEFORE firing t2, so the
	// fixed-window throttle counter has committed the first send (count=1)
	// before the second send is gated — the throttler is best-effort and a
	// concurrent pair could otherwise both read count=0.
	trigger1("t1")
	pollFor(1)
	trigger1("t2")
	msgs := pollFor(2)

	statuses := []string{msgs[0].Status, msgs[1].Status}
	sort.Strings(statuses)
	if statuses[0] != "sent" || statuses[1] != "throttled" {
		t.Errorf("statuses = %v, want exactly one %q and one %q", statuses, "sent", "throttled")
	}
}
