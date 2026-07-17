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
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestPreference_SkipsDisabledChannel is the wave-C proof: the in_app delivery
// handler is wired WITH a real preferences.Service checker. A subscriber with
// the in_app channel globally disabled triggers the workflow; the recorded
// message MUST land as Status "skipped" (never "sent"). A sibling positive
// control (TestPreference_SendsWhenEnabled) proves the skip is caused by the
// preference and not a bug in the delivery path.
func TestPreference_SkipsDisabledChannel(t *testing.T) {
	srv, scope, prefs, authHeader := newPrefHarness(t)

	ctx := context.Background()

	// POST /v1/subscribers -> 201.
	subBody := `{"subscriberId":"user_skip","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// Disable the in_app channel globally for the subscriber.
	if err := prefs.SetGlobal(ctx, scope, "user_skip", map[string]bool{"in_app": false}); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}

	// Trigger -> processed.
	trigBody := `{"name":"welcome","to":{"subscriberId":"user_skip"},"payload":{"itemCount":3},"transactionId":"stx-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}
	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	if !resp.Acknowledged || resp.Status != "processed" {
		t.Errorf("trigger: Acknowledged=%v Status=%q, want true/processed", resp.Acknowledged, resp.Status)
	}

	msg := pollMessage(t, srv.URL, authHeader, "user_skip")
	msgJSON, _ := json.Marshal(msg)
	t.Logf("skip message: %s", msgJSON)

	if msg.Status != "skipped" {
		t.Errorf("skip: Status = %q, want %q", msg.Status, "skipped")
	}
	if msg.Status == "sent" {
		t.Errorf("skip: channel was sent despite disabled preference")
	}
	if msg.TransactionID != "stx-1" {
		t.Errorf("skip: TransactionID = %q, want %q", msg.TransactionID, "stx-1")
	}
}

// TestPreference_SendsWhenEnabled is the positive control: the same wiring with
// NO disabling preference delivers the in_app message as Status "sent".
func TestPreference_SendsWhenEnabled(t *testing.T) {
	srv, _, _, authHeader := newPrefHarness(t)

	subBody := `{"subscriberId":"user_send","firstName":"Bob"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"welcome","to":{"subscriberId":"user_send"},"payload":{"itemCount":3},"transactionId":"stx-2"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}

	msg := pollMessage(t, srv.URL, authHeader, "user_send")
	msgJSON, _ := json.Marshal(msg)
	t.Logf("send message: %s", msgJSON)

	if msg.Status != "sent" {
		t.Errorf("send: Status = %q, want %q", msg.Status, "sent")
	}
	if msg.TransactionID != "stx-2" {
		t.Errorf("send: TransactionID = %q, want %q", msg.TransactionID, "stx-2")
	}
}

// newPrefHarness boots a real sidecar, store, engine (with the preferences-aware
// in_app handler), and HTTP server, and publishes the "welcome" in_app workflow.
func newPrefHarness(t *testing.T) (srv *httptest.Server, scope domain.Scope, prefs *preferences.Service, authHeader string) {
	t.Helper()
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

	prefs = preferences.New(st)
	eng.Handle("send.in_app", delivery.New(st).WithPreferences(prefs).HandleInApp)
	eng.StartWorkers(ctx)

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, sc, bootErr := id.Bootstrap(ctx, "Acme", "dev")
	if bootErr != nil {
		t.Fatalf("Bootstrap: %v", bootErr)
	}
	scope = sc

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

	srv = httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox}))
	t.Cleanup(srv.Close)

	authHeader = "ApiKey " + key
	return srv, scope, prefs, authHeader
}

// pollMessage polls the in_app message feed until at least one message arrives.
func pollMessage(t *testing.T, baseURL, authHeader, subscriberID string) domain.Message {
	t.Helper()
	feedURL := baseURL + "/v1/messages?subscriberId=" + subscriberID + "&channel=in_app"
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
		if len(got) >= 1 {
			return got[0]
		}
		t.Logf("poll: 0 messages yet for %s (feed=%s)", subscriberID, body)
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no in_app message arrived within 20s for %s; last feed JSON: %s", subscriberID, lastBody)
	return domain.Message{}
}
