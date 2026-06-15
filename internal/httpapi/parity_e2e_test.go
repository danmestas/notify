package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestInAppParity_EndToEnd is the in-app slice parity proof: a real dagnats
// sidecar, a real temp SQLite store, and the real HTTP surface drive the full
// scenario (subscriber -> trigger -> delivered in_app message) and
// assert the two recorded-contract oracles in docs/proof/in-app-parity-oracle.md.
func TestInAppParity_EndToEnd(t *testing.T) {
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

	// Register the in_app delivery handler before workers start.
	eng.Handle("send.in_app", delivery.New(st).HandleInApp)
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

	// Author + publish a 1-step in_app workflow via the service (not HTTP).
	controls := json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`)
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

	// POST /v1/subscribers -> 201.
	subBody := `{"subscriberId":"user_123","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// POST /v1/events/trigger -> 201 + TriggerResponse (Oracle 1).
	trigBody := `{"name":"welcome-flow","to":{"subscriberId":"user_123"},"payload":{"itemCount":3},"transactionId":"tx-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}
	t.Logf("trigger response: %s", respJSON)

	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	// --- ORACLE 1 ---
	if !resp.Acknowledged {
		t.Errorf("Oracle 1: Acknowledged = false, want true")
	}
	if resp.Status != "processed" {
		t.Errorf("Oracle 1: Status = %q, want %q", resp.Status, "processed")
	}
	if resp.TransactionID != "tx-1" {
		t.Errorf("Oracle 1: TransactionID = %q, want %q", resp.TransactionID, "tx-1")
	}

	// Poll GET /v1/messages until the delivered in_app message arrives.
	feedURL := srv.URL + "/v1/messages?subscriberId=user_123&channel=in_app"
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
		// Surface the live run state while we wait, for debugging.
		t.Logf("poll: 0 messages yet (feed=%s)", body)
		time.Sleep(300 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatalf("Oracle 2: no in_app message arrived within 20s; last feed JSON: %s", lastBody)
	}

	msg := msgs[0]
	msgJSON, _ := json.Marshal(msg)
	t.Logf("message: %s", msgJSON)

	// --- ORACLE 2 ---
	if msg.Channel != "in_app" {
		t.Errorf("Oracle 2: Channel = %q, want %q", msg.Channel, "in_app")
	}
	if msg.Subject != "Hi Ada" {
		t.Errorf("Oracle 2: Subject = %q, want %q", msg.Subject, "Hi Ada")
	}
	if msg.Content != "You have 3 items" {
		t.Errorf("Oracle 2: Content = %q, want %q", msg.Content, "You have 3 items")
	}
	if msg.Status != "sent" {
		t.Errorf("Oracle 2: Status = %q, want %q", msg.Status, "sent")
	}
	if msg.Seen {
		t.Errorf("Oracle 2: Seen = true, want false")
	}
	if msg.Read {
		t.Errorf("Oracle 2: Read = true, want false")
	}
	if msg.TransactionID != "tx-1" {
		t.Errorf("Oracle 2: TransactionID = %q, want %q", msg.TransactionID, "tx-1")
	}
}

// doPost issues an authenticated JSON POST and returns the status and body.
func doPost(t *testing.T, url, authHeader, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// doGet issues an authenticated GET and returns the status and body.
func doGet(t *testing.T, url, authHeader string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
