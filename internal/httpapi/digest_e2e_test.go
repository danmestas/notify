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

// TestDigest_BatchesEvents proves the DIGEST slice end-to-end: a workflow whose
// first step is "digest" (a 1s Sleep window) followed by an in_app step batches
// THREE rapid triggers for the same (env, subscriber, workflow) into ONE
// downstream run. Exactly one trigger claims ownership and starts the run; the
// other two merge silently. After the window fires, the in_app handler
// self-sources, drains the three merged events, and renders {{digest.count}} =>
// a single "You have 3 new items" message (NOT three separate messages).
func TestDigest_BatchesEvents(t *testing.T) {
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
	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).WithDigester(bus).HandleInApp)
	eng.StartWorkers(ctx)

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng).WithDigester(bus)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Two-step workflow: a 1s digest window, then an in_app step whose body
	// renders the merged event count.
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Digested",
		TriggerIdentifier: "digested",
		Steps: []domain.Step{
			{
				StepID:        "dg",
				Type:          "digest",
				ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`),
			},
			{
				StepID:        "S1",
				Type:          "in_app",
				ControlValues: json.RawMessage(`{"subject":"Digest","body":"You have {{digest.count}} new items"}`),
			},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "digested"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"dg_user"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// Three rapid triggers, same subscriber, distinct transaction ids. They all
	// land well within the 1s window, so they merge into one run.
	for _, tx := range []string{"d1", "d2", "d3"} {
		trigBody := `{"name":"digested","to":{"subscriberId":"dg_user"},"payload":{},"transactionId":"` + tx + `"}`
		if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger %s: status=%d body=%s", tx, status, body)
		}
	}

	feedURL := srv.URL + "/v1/messages?subscriberId=dg_user&channel=in_app"
	sentMsgs := func(body string) []domain.Message {
		var got []domain.Message
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode messages: %v (body=%s)", err, body)
		}
		var sent []domain.Message
		for _, m := range got {
			if m.Status == "sent" {
				sent = append(sent, m)
			}
		}
		return sent
	}

	var sent []domain.Message
	var lastBody string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, body := doGet(t, feedURL, authHeader)
		lastBody = body
		if status != http.StatusOK {
			t.Fatalf("GET /v1/messages: status=%d body=%s", status, body)
		}
		if s := sentMsgs(body); len(s) >= 1 {
			sent = s
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(sent) == 0 {
		t.Fatalf("no sent in_app message arrived within 20s; last feed JSON: %s", lastBody)
	}

	t.Logf("digest delivered %d sent in_app message(s) (want exactly 1)", len(sent))
	if len(sent) != 1 {
		t.Fatalf("digest NOT batched: %d sent in_app messages, want exactly 1 (3 rapid triggers should merge into one run); body=%s", len(sent), lastBody)
	}
	if sent[0].Content != "You have 3 new items" {
		t.Errorf("Content = %q, want %q", sent[0].Content, "You have 3 new items")
	}
	if sent[0].Status != "sent" {
		t.Errorf("Status = %q, want sent", sent[0].Status)
	}
}
