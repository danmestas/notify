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

// TestTrigger_ResponseCarriesNotificationID proves the wire contract of
// POST /v1/events/trigger: the literal key notificationIds lists the id the
// trigger wrote, and that id reads the delivered message back from
// GET /v1/notifications/{id}/messages.
func TestTrigger_ResponseCarriesNotificationID(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	eng, err := engine.New(natsURL)
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

	eng.Handle("notify.send.in_app", delivery.New(st).HandleInApp)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Ping",
		TriggerIdentifier: "ping",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: json.RawMessage(`{"subject":"Hi","body":"hello"}`),
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "ping"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Store: st}))
	defer srv.Close()
	authHeader := "ApiKey " + key

	status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader,
		`{"name":"ping","to":{"subscriberId":"u1"},"payload":{},"transactionId":"tx-http-ids"}`)
	if status != http.StatusCreated {
		t.Fatalf("trigger status=%d body=%s", status, body)
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids, ok := wire["notificationIds"].([]any)
	if !ok || len(ids) != 1 {
		t.Fatalf("notificationIds = %v, want exactly one id (body=%s)", wire["notificationIds"], body)
	}
	nid, _ := ids[0].(string)

	deadline := time.Now().Add(20 * time.Second)
	for {
		status, msgs := doGet(t, srv.URL+"/v1/notifications/"+nid+"/messages", authHeader)
		var list []map[string]any
		if status == http.StatusOK && json.Unmarshal([]byte(msgs), &list) == nil && len(list) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET messages for %s = %d %s, want a message", nid, status, msgs)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
