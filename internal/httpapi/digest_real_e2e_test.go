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

// digestHarness boots a real sidecar + engine + store + bus, registers the
// in_app and digest.wait handlers, and returns the wired control-plane services
// plus an HTTP test server. It is the shared setup for the real-digest e2e tests
// (events injection + scheduled window), mirroring TestDigest_BatchesEvents.
type digestHarness struct {
	srv        *httptest.Server
	authHeader string
	ctx        context.Context
	auth       *authoring.Service
	scope      domain.Scope
}

func newDigestHarness(t *testing.T) *digestHarness {
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

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).WithDigester(bus).HandleInApp)
	eng.Handle("digest.wait", delivery.NewDigestWait(st).HandleDigestWait)
	// Guard/smoke: the scheduled-digest window is dead without this handler, so
	// assert it is registered before the workers start (a wiring regression
	// would silently leave digest.wait tasks unclaimed forever).
	if !eng.Handles("digest.wait") {
		t.Fatal("digest.wait handler not registered")
	}
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

	h := &digestHarness{authHeader: "ApiKey " + key}
	h.srv = httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox}))
	t.Cleanup(h.srv.Close)
	h.ctx = ctx
	h.auth = auth
	h.scope = scope
	return h
}

func (h *digestHarness) createSubscriber(t *testing.T, id string) {
	t.Helper()
	if status, body := doPost(t, h.srv.URL+"/v1/subscribers", h.authHeader, `{"subscriberId":"`+id+`"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}
}

// pollSent polls the in_app feed until at least one "sent" message arrives or
// the deadline passes, returning the sent messages.
func (h *digestHarness) pollSent(t *testing.T, subscriberID string) []domain.Message {
	t.Helper()
	feedURL := h.srv.URL + "/v1/messages?subscriberId=" + subscriberID + "&channel=in_app"
	deadline := time.Now().Add(25 * time.Second)
	var lastBody string
	for time.Now().Before(deadline) {
		status, body := doGet(t, feedURL, h.authHeader)
		lastBody = body
		if status != http.StatusOK {
			t.Fatalf("GET /v1/messages: status=%d body=%s", status, body)
		}
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
		if len(sent) >= 1 {
			return sent
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no sent in_app message arrived within 25s; last feed JSON: %s", lastBody)
	return nil
}

// TestDigest_InjectsEventsAndCount_InApp proves a regular digest exposes BOTH
// {{digest.count}} and an iterable {{digest.events}} to the downstream template:
// three rapid triggers carrying {"n":1},{"n":2},{"n":3} merge into ONE in_app
// send whose body renders the count (3) and each event's field (123).
func TestDigest_InjectsEventsAndCount_InApp(t *testing.T) {
	h := newDigestHarness(t)
	if _, err := h.auth.Create(h.ctx, h.scope, authoring.WorkflowInput{
		Name:              "Digested events",
		TriggerIdentifier: "digested-events",
		Steps: []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"regular","amount":1,"unit":"seconds"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Digest","body":"count={{digest.count}} items={{#each digest.events}}{{this.n}}{{/each}}"}`)},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := h.auth.Publish(h.ctx, h.scope, "digested-events"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	h.createSubscriber(t, "ev_user")
	for i, tx := range []string{"e1", "e2", "e3"} {
		body := `{"name":"digested-events","to":{"subscriberId":"ev_user"},"payload":{"n":` + string(rune('1'+i)) + `},"transactionId":"` + tx + `"}`
		if status, rb := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader, body); status != http.StatusCreated {
			t.Fatalf("trigger %s: status=%d body=%s", tx, status, rb)
		}
	}

	sent := h.pollSent(t, "ev_user")
	if len(sent) != 1 {
		t.Fatalf("digest NOT batched: %d sent messages, want exactly 1", len(sent))
	}
	if sent[0].Content != "count=3 items=123" {
		t.Errorf("Content = %q, want %q", sent[0].Content, "count=3 items=123")
	}
}

// TestDigest_Scheduled_WakesAtCronTick proves a scheduled (cron) digest window:
// the digest.wait handler sleeps until the next every-second tick, then the
// downstream in_app self-sources and drains the accumulated events. Three rapid
// triggers merge into ONE send with {{digest.count}} = 3.
func TestDigest_Scheduled_WakesAtCronTick(t *testing.T) {
	h := newDigestHarness(t)
	if _, err := h.auth.Create(h.ctx, h.scope, authoring.WorkflowInput{
		Name:              "Scheduled digest",
		TriggerIdentifier: "scheduled-digest",
		Steps: []domain.Step{
			{StepID: "dg", Type: "digest", ControlValues: json.RawMessage(`{"type":"scheduled","cron":"* * * * * *"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Digest","body":"You have {{digest.count}} new items"}`)},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := h.auth.Publish(h.ctx, h.scope, "scheduled-digest"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	h.createSubscriber(t, "sc_user")
	for _, tx := range []string{"s1", "s2", "s3"} {
		body := `{"name":"scheduled-digest","to":{"subscriberId":"sc_user"},"payload":{},"transactionId":"` + tx + `"}`
		if status, rb := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader, body); status != http.StatusCreated {
			t.Fatalf("trigger %s: status=%d body=%s", tx, status, rb)
		}
	}

	sent := h.pollSent(t, "sc_user")
	if len(sent) != 1 {
		t.Fatalf("scheduled digest NOT batched: %d sent messages, want exactly 1", len(sent))
	}
	if sent[0].Content != "You have 3 new items" {
		t.Errorf("Content = %q, want %q", sent[0].Content, "You have 3 new items")
	}
}
