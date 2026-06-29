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

// TestCheckThrottle_StepHandler_SecondRunDropped is the proof that the
// check.throttle ENGINE STEP (not the per-channel embedded throttle) gates
// entire workflow runs: a real dagnats sidecar, real SQLite, and the real
// trigger+engine stack drive two runs for the same subscriber against a
// workflow whose FIRST step is Type:"throttle" (limit=1) followed by an in_app
// step. The first run must complete (in_app fires, 1 message written); the
// second run must be permanently failed by check.throttle with a
// NonRetryableError so the in_app step NEVER runs and NO second message row is
// written.
//
// This test fails if ThrottleHandler.Handle is wrong (e.g. wrong key, wrong
// counter logic) or if the eng.Handle(delivery.TaskThrottle, ...) registration
// is absent (the Handles guard below catches handler-desync before workers
// start).
func TestCheckThrottle_StepHandler_SecondRunDropped(t *testing.T) {
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

	// Register the check.throttle step handler and the in_app channel handler.
	eng.Handle(delivery.TaskThrottle, delivery.NewThrottleHandler(st, bus).Handle)
	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).HandleInApp)

	// LOW guard: assert check.throttle is wired before workers start so that
	// removing the eng.Handle(delivery.TaskThrottle, ...) line causes an
	// immediate, readable failure here rather than a silent engine timeout.
	if !eng.Handles(delivery.TaskThrottle) {
		t.Fatalf("handler desync: %q not registered; add eng.Handle(%q, ...) before StartWorkers", delivery.TaskThrottle, delivery.TaskThrottle)
	}
	eng.StartWorkers(ctx)

	masterKey := bytes.Repeat([]byte{0x2b}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)

	key, scope, err := id.Bootstrap(ctx, "ThrottleTest", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	_ = key

	// Workflow: throttle step (limit=1, 1-hour window) → in_app step.
	// Use a large window so both triggers fall in the same epoch bucket; the
	// behavior under test is "limit=1 drops the second run", not the window.
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "ThrottledWorkflow",
		TriggerIdentifier: "throttle-step-flow",
		Steps: []domain.Step{
			{StepID: "T1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":1,"windowSec":3600}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Hi","body":"throttle step e2e"}`)},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "throttle-step-flow"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	// Upsert subscriber (trigger.Ingest upserts inline, but seed explicitly so
	// the row exists before the first trigger for determinism).
	if _, err := aud.Upsert(ctx, scope, audience.SubscriberInput{SubscriberID: "sub_throttle"}); err != nil {
		t.Fatalf("Upsert subscriber: %v", err)
	}

	// triggerAndGetRunID fires one trigger and returns the engine run ID by
	// reading the notification row that trigger.Ingest creates synchronously.
	triggerAndGetRunID := func(txID string) string {
		t.Helper()
		resp, tErr := trg.Ingest(ctx, scope, trigger.TriggerRequest{
			Name:          "throttle-step-flow",
			To:            trigger.ToField{SubscriberID: "sub_throttle"},
			TransactionID: txID,
		})
		if tErr != nil {
			t.Fatalf("Ingest %s: %v", txID, tErr)
		}
		if !resp.Acknowledged {
			t.Fatalf("Ingest %s not acknowledged: status=%q errors=%v", txID, resp.Status, resp.Error)
		}
		page, lErr := st.Notifications().List(ctx, scope, store.NotificationFilter{TransactionID: txID}, "", 10)
		if lErr != nil || len(page.Items) == 0 {
			t.Fatalf("Notifications.List for tx %s: err=%v items=%d", txID, lErr, len(page.Items))
		}
		return page.Items[0].RunID
	}

	// pollRunTerminal waits up to 20 s for a run to reach a terminal state.
	pollRunTerminal := func(runID string) engine.RunSnapshot {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		var snap engine.RunSnapshot
		for time.Now().Before(deadline) {
			s, gerr := eng.GetRun(ctx, runID)
			if gerr == nil {
				snap = s
				if snap.IsTerminal() {
					return snap
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("run %s never reached terminal state; last status: %q", runID, snap.Status)
		return snap
	}

	// --- First trigger: must pass throttle (count 0→1) and deliver 1 message.
	runID1 := triggerAndGetRunID("tx-throttle-step-1")
	snap1 := pollRunTerminal(runID1)
	if !snap1.Succeeded() {
		t.Fatalf("first run %s: expected completed, got status=%q", runID1, snap1.Status)
	}

	msgs1, err := st.Messages().List(ctx, scope, "sub_throttle", "in_app")
	if err != nil {
		t.Fatalf("Messages.List after first run: %v", err)
	}
	if len(msgs1) != 1 {
		t.Fatalf("after first trigger: want 1 in_app message, got %d", len(msgs1))
	}
	if msgs1[0].Status != "sent" {
		t.Errorf("first message status = %q, want \"sent\"", msgs1[0].Status)
	}

	// --- Second trigger: check.throttle must fire NonRetryableError (count 1
	// >= limit 1), permanently failing the run. The in_app step MUST NOT run,
	// so no second message row is written.
	runID2 := triggerAndGetRunID("tx-throttle-step-2")
	snap2 := pollRunTerminal(runID2)
	if snap2.Succeeded() {
		t.Fatalf("second run %s: expected failed (throttled by check.throttle), got status=%q", runID2, snap2.Status)
	}

	msgs2, err := st.Messages().List(ctx, scope, "sub_throttle", "in_app")
	if err != nil {
		t.Fatalf("Messages.List after second run: %v", err)
	}
	if len(msgs2) != 1 {
		t.Fatalf("after second trigger: want still 1 in_app message (second run dropped), got %d", len(msgs2))
	}
}

// TestThrottle_SecondSendThrottled is the per-step rate-limit proof (legacy
// per-channel embedded throttle path): a real dagnats sidecar, real temp
// SQLite, and the real HTTP surface drive two triggers for the same subscriber
// against an in_app workflow whose step configures throttle
// {limit:1,windowSec:60}. Exactly one message is "sent" and the second is
// "throttled". This exercises the OLD WithThrottle channel-level gate, NOT the
// check.throttle step handler; see TestCheckThrottle_StepHandler_SecondRunDropped
// for the step-level proof.
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

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil, nil, nil, nil))
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
