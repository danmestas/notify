package httpapi_test

import (
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

// TestTranslation_LocalizedInApp proves the i18n slice end-to-end: an in_app
// body authored as {{t "greeting"}} renders the subscriber-locale string
// seeded in the translations store. A subscriber with locale "es" and a seeded
// (es, greeting -> "Hola") yields content "Hola Ada".
func TestTranslation_LocalizedInApp(t *testing.T) {
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

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = 0x2a
	}
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Seed the Spanish translation for "greeting".
	if err := st.Translations().Upsert(ctx, scope, "es", "greeting", "Hola"); err != nil {
		t.Fatalf("seed translation: %v", err)
	}

	controls, _ := json.Marshal(map[string]string{
		"subject": "Welcome",
		"body":    `{{t "greeting"}} {{subscriber.firstName}}`,
	})
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Localized",
		TriggerIdentifier: "loc-flow",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "loc-flow"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	// Subscriber carries locale "es".
	subBody := `{"subscriberId":"loc_user","firstName":"Ada","locale":"es"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"loc-flow","to":{"subscriberId":"loc_user"},"payload":{},"transactionId":"tx-loc"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	feedURL := srv.URL + "/v1/messages?subscriberId=loc_user&channel=in_app"
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

	if got := msgs[0].Content; got != "Hola Ada" {
		t.Fatalf("localized content = %q, want %q", got, "Hola Ada")
	}
}
