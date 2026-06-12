package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// ntfyCapture records the requests a stub ntfy server receives (thread-safe).
type ntfyCapture struct {
	mu   sync.Mutex
	reqs []ntfyRequest
}

type ntfyRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

func (c *ntfyCapture) add(r ntfyRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
}

func (c *ntfyCapture) all() []ntfyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ntfyRequest, len(c.reqs))
	copy(out, c.reqs)
	return out
}

// TestPushParity_EndToEnd is the push slice proof: a real dagnats sidecar, a
// real temp SQLite store, the real HTTP surface, and the REAL ntfy client
// pointed at a stub ntfy server. It drives integration create (HTTP) ->
// subscriber + topic credentials (HTTP) -> trigger (HTTP) -> delivered push
// message, asserting the persisted Message and the exact ntfy wire.
func TestPushParity_EndToEnd(t *testing.T) {
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

	// Stub ntfy server: records the wire and answers like ntfy 2.x.
	capt := &ntfyCapture{}
	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capt.add(ntfyRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ntfy-e2e-1","topic":"agent-alerts"}`))
	}))
	defer ntfySrv.Close()

	masterKey := bytesRepeat(0x2a, 32)
	conn := connectivity.New(st, masterKey)

	// Register the push delivery handler before workers start: the resolver is
	// the real connectivity service, so the path under test includes encrypted
	// credential storage and the real ntfy client.
	eng.Handle("send.push", delivery.NewPush(st, conn).HandlePush)
	eng.StartWorkers(ctx)

	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Author + publish a 1-step push workflow via the service (not HTTP).
	controls := json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items","priority":4,"tags":["warning","skull"],"markdown":true}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Agent Alert",
		TriggerIdentifier: "agent-alert",
		Steps: []domain.Step{{
			StepID:        "P1",
			Type:          "push",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "agent-alert"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, conn, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	// POST /v1/integrations/push -> 201 (ntfy server creds, sealed at rest).
	integBody := `{"identifier":"primary-ntfy","baseUrl":"` + ntfySrv.URL + `","token":"tk_e2e","primary":true}`
	if status, body := doPost(t, srv.URL+"/v1/integrations/push", authHeader, integBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/integrations/push: status=%d body=%s", status, body)
	}

	// POST /v1/subscribers -> 201.
	subBody := `{"subscriberId":"user_p","firstName":"Ada","email":"ada@acme.io"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// PUT /v1/subscribers/user_p/credentials -> 204 (the ntfy topic).
	credBody := `{"providerId":"ntfy","channel":"push","credentials":{"topic":"agent-alerts"}}`
	if status, body := doPut(t, srv.URL+"/v1/subscribers/user_p/credentials", authHeader, credBody); status != http.StatusNoContent {
		t.Fatalf("PUT /v1/subscribers/user_p/credentials: status=%d body=%s", status, body)
	}

	// POST /v1/events/trigger -> 201 + TriggerResponse.
	trigBody := `{"name":"agent-alert","to":{"subscriberId":"user_p"},"payload":{"itemCount":3},"transactionId":"ptx-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}
	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	if !resp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}

	// Poll GET /v1/messages until the delivered push message arrives.
	feedURL := srv.URL + "/v1/messages?subscriberId=user_p&channel=push"
	var msgs []domain.Message
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
			msgs = got
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatalf("no push message arrived within 20s; last feed JSON: %s", lastBody)
	}

	// --- persisted Message oracle ---
	msg := msgs[0]
	if msg.Channel != "push" {
		t.Errorf("Channel = %q, want %q", msg.Channel, "push")
	}
	if msg.ProviderID != "ntfy" {
		t.Errorf("ProviderID = %q, want %q", msg.ProviderID, "ntfy")
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
	if msg.TransactionID != "ptx-1" {
		t.Errorf("TransactionID = %q, want %q", msg.TransactionID, "ptx-1")
	}

	// --- stub ntfy oracle: exactly one POST with the pinned wire ---
	reqs := capt.all()
	if len(reqs) != 1 {
		t.Fatalf("stub ntfy received %d requests, want 1 (%+v)", len(reqs), reqs)
	}
	nr := reqs[0]
	if nr.method != http.MethodPost {
		t.Errorf("ntfy method = %q, want POST", nr.method)
	}
	if nr.path != "/agent-alerts" {
		t.Errorf("ntfy path = %q, want /agent-alerts", nr.path)
	}
	if h := nr.header.Get("Authorization"); h != "Bearer tk_e2e" {
		t.Errorf("Authorization = %q, want %q", h, "Bearer tk_e2e")
	}
	if h := nr.header.Get("X-Title"); h != "Hi Ada" {
		t.Errorf("X-Title = %q, want %q", h, "Hi Ada")
	}
	if h := nr.header.Get("X-Priority"); h != "4" {
		t.Errorf("X-Priority = %q, want %q", h, "4")
	}
	if h := nr.header.Get("X-Tags"); h != "warning,skull" {
		t.Errorf("X-Tags = %q, want %q", h, "warning,skull")
	}
	if h := nr.header.Get("X-Markdown"); h != "yes" {
		t.Errorf("X-Markdown = %q, want %q", h, "yes")
	}
	if nr.body != "You have 3 items" {
		t.Errorf("ntfy body = %q, want %q", nr.body, "You have 3 items")
	}
}

// doPut issues an authenticated PUT with a JSON body and returns status + body.
func doPut(t *testing.T, url, authHeader, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("new PUT %s: %v", url, err)
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read PUT %s body: %v", url, err)
	}
	return resp.StatusCode, string(b)
}
