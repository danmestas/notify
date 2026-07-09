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
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// httpActionHarness boots a real sidecar + engine + store + bus, registers the
// http_request action handler and the in_app channel handler, and returns the
// wired control-plane services plus an HTTP test server.
type httpActionHarness struct {
	srv        *httptest.Server
	authHeader string
	ctx        context.Context
	auth       *authoring.Service
	scope      domain.Scope
}

func newHTTPActionHarness(t *testing.T) *httpActionHarness {
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

	// NewWithClient with a bare http.Client bypasses the SSRF guard so the
	// workflow under test can reach httptest servers on loopback. The SSRF guard
	// itself is exercised in internal/delivery/httpaction/ssrfguard_test.go.
	eng.Handle("http_request", httpaction.NewWithClient(st, &http.Client{}).Handle)
	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).HandleInApp)

	// Guard: assert the http_request action handler is wired before workers
	// start, so removing the eng.Handle("http_request", ...) registration causes
	// an immediate, readable failure here rather than a silent engine timeout.
	if !eng.Handles("http_request") {
		t.Fatal(`handler desync: "http_request" not registered; add eng.Handle("http_request", ...) before StartWorkers`)
	}
	eng.StartWorkers(ctx)

	masterKey := bytes.Repeat([]byte{0x2c}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "HTTPAction", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	h := &httpActionHarness{authHeader: "ApiKey " + key}
	h.srv = httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	t.Cleanup(h.srv.Close)
	h.ctx = ctx
	h.auth = auth
	h.scope = scope
	return h
}

func (h *httpActionHarness) createSubscriber(t *testing.T, id string) {
	t.Helper()
	if status, body := doPost(t, h.srv.URL+"/v1/subscribers", h.authHeader, `{"subscriberId":"`+id+`"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}
}

// pollMessage polls the in_app feed until at least one message in any terminal
// status arrives, returning it. Unlike pollSent it accepts "skipped" too, so the
// filter-gate test can assert a gated step's status.
func (h *httpActionHarness) pollMessage(t *testing.T, subscriberID string) domain.Message {
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
		if len(got) >= 1 {
			return got[0]
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no in_app message arrived within 25s; last feed JSON: %s", lastBody)
	return domain.Message{}
}

// TestHTTPAction_DownstreamInApp_UsesStepOutput is the end-to-end proof through
// the real trigger+engine+dagnats stack: a [http_request -> in_app] workflow
// makes the call, captures statusCode into step_outputs, and the downstream
// in_app body renders {{steps.h1.statusCode}} as the live 200.
func TestHTTPAction_DownstreamInApp_UsesStepOutput(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	h := newHTTPActionHarness(t)
	cv := json.RawMessage(`{"url":"` + target.URL + `","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`)
	if _, err := h.auth.Create(h.ctx, h.scope, authoring.WorkflowInput{
		Name:              "Call then notify",
		TriggerIdentifier: "call-then-notify",
		Steps: []domain.Step{
			{StepID: "h1", Type: "http_request", ControlValues: cv},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"R","body":"Status: {{steps.h1.statusCode}}"}`)},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := h.auth.Publish(h.ctx, h.scope, "call-then-notify"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	h.createSubscriber(t, "ha_user")
	body := `{"name":"call-then-notify","to":{"subscriberId":"ha_user"},"payload":{},"transactionId":"ha1"}`
	if status, rb := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader, body); status != http.StatusCreated {
		t.Fatalf("trigger: status=%d body=%s", status, rb)
	}

	m := h.pollMessage(t, "ha_user")
	if m.Status != "sent" {
		t.Fatalf("in_app status = %q, want sent", m.Status)
	}
	if m.Content != "Status: 200" {
		t.Errorf("Content = %q, want %q", m.Content, "Status: 200")
	}
}

// TestHTTPAction_FilterGatesInApp proves the filter-gate path end-to-end: an
// in_app step conditioned on the upstream http statusCode being 403 is SKIPPED
// (fail-closed) when the live response is 200.
func TestHTTPAction_FilterGatesInApp(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	h := newHTTPActionHarness(t)
	cv := json.RawMessage(`{"url":"` + target.URL + `","method":"GET","headers":"{}","body":"","timeoutSec":30,"failurePolicy":"fail_closed"}`)
	if _, err := h.auth.Create(h.ctx, h.scope, authoring.WorkflowInput{
		Name:              "Gated notify",
		TriggerIdentifier: "gated-notify",
		Steps: []domain.Step{
			{StepID: "h1", Type: "http_request", ControlValues: cv},
			{
				StepID: "S1", Type: "in_app",
				ControlValues: json.RawMessage(`{"subject":"R","body":"hi"}`),
				Filters:       json.RawMessage(`[{"combinator":"AND","conditions":[{"on":"h1","field":"statusCode","operator":"EQUAL","value":"403"}]}]`),
			},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := h.auth.Publish(h.ctx, h.scope, "gated-notify"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	h.createSubscriber(t, "gate_user")
	body := `{"name":"gated-notify","to":{"subscriberId":"gate_user"},"payload":{},"transactionId":"g1"}`
	if status, rb := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader, body); status != http.StatusCreated {
		t.Fatalf("trigger: status=%d body=%s", status, rb)
	}

	m := h.pollMessage(t, "gate_user")
	if m.Status != "skipped" {
		t.Errorf("in_app status = %q, want skipped (statusCode 200 != 403 condition)", m.Status)
	}
}
