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
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestLayout_WrapsContent proves the layouts slice end-to-end: a step that
// references a layoutKey has its rendered body injected as {{content}} into the
// layout's content template before persistence. Real sidecar + temp SQLite +
// HTTP surface, mirroring the in-app parity harness.
func TestLayout_WrapsContent(t *testing.T) {
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

	// Seed a layout wrapping its {{content}} placeholder in brackets.
	if err := st.Layouts().Upsert(ctx, scope, "default", "Default", "[ {{content}} ]"); err != nil {
		t.Fatalf("Layouts.Upsert: %v", err)
	}

	// Author + publish a 1-step in_app workflow that references the layout.
	controls := json.RawMessage(`{"subject":"Hi","body":"Hello {{subscriber.firstName}}","layoutKey":"default"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Layout Welcome",
		TriggerIdentifier: "layout-flow",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "layout-flow"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil, nil, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	subBody := `{"subscriberId":"lay_user","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"layout-flow","to":{"subscriberId":"lay_user"},"payload":{},"transactionId":"tx-lay"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	feedURL := srv.URL + "/v1/messages?subscriberId=lay_user&channel=in_app"
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
		t.Fatalf("no in_app message arrived within 20s; last feed JSON: %s", lastBody)
	}

	msg := msgs[0]
	if msg.Content != "[ Hello Ada ]" {
		t.Errorf("Content = %q, want %q", msg.Content, "[ Hello Ada ]")
	}
	// Subject is never wrapped by the layout.
	if msg.Subject != "Hi" {
		t.Errorf("Subject = %q, want %q", msg.Subject, "Hi")
	}
}
