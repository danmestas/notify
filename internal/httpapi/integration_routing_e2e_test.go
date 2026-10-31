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

// TestIntegrationRouting_TwoChatSteps_EndToEnd is the #314 e2e proof: a real
// dagnats sidecar, a real temp SQLite store, and the real connectivity.Service
// drive a two-step chat workflow where each step targets a different
// integration — the first step sends through the channel's primary
// integration (no "integration" control), the second names a second
// integration explicitly. Each step's persisted message carries its own
// provider_id/integration_id and each httptest fake webhook receives exactly
// one POST.
func TestIntegrationRouting_TwoChatSteps_EndToEnd(t *testing.T) {
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

	masterKey := bytesRepeat(0x2a, 32)

	// WithHTTPClient bypasses the SSRF guard deliveryClient() otherwise applies
	// to outbound chat webhook requests (it blocks loopback addresses in
	// production) so the test's httptest.Server fakes, which are loopback-only,
	// are reachable.
	conn := connectivity.New(st, masterKey, connectivity.WithHTTPClient(http.DefaultClient))

	// Two independent Discord-style webhook fakes standing in for two distinct
	// chat integrations in the same scope.
	primaryReceived := make(chan struct{}, 2)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryReceived <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(primary.Close)

	secondaryReceived := make(chan struct{}, 2)
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryReceived <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(secondary.Close)

	// Register the chat handler, backed by the real connectivity.Service, before
	// workers start.
	eng.Handle("send.chat", delivery.NewChat(st, conn).HandleChat)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
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

	// Seed two chat integrations: "webhook-primary" is the channel's primary
	// (step C1 sends through it with no "integration" control); "webhook-secondary"
	// is a non-primary integration that step C2 names explicitly.
	if _, err := conn.CreateDiscord(ctx, scope, "webhook-primary", connectivity.DiscordConfig{
		WebhookURL: primary.URL,
	}, true); err != nil {
		t.Fatalf("CreateDiscord(primary): %v", err)
	}
	if _, err := conn.CreateDiscord(ctx, scope, "webhook-secondary", connectivity.DiscordConfig{
		WebhookURL: secondary.URL,
	}, false); err != nil {
		t.Fatalf("CreateDiscord(secondary): %v", err)
	}

	// Two-step chat workflow: C1 (implicit primary) -> C2 (explicit
	// "webhook-secondary" integration).
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Dual Chat",
		TriggerIdentifier: "dual-chat",
		Steps: []domain.Step{
			{
				StepID:        "C1",
				Type:          "chat",
				ControlValues: json.RawMessage(`{"body":"Primary hello {{subscriber.firstName}}"}`),
			},
			{
				StepID:        "C2",
				Type:          "chat",
				ControlValues: json.RawMessage(`{"body":"Secondary hello {{subscriber.firstName}}","integration":"webhook-secondary"}`),
			},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "dual-chat"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox}))
	defer srv.Close()

	authHeader := "ApiKey " + key

	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, `{"subscriberId":"dual_user","firstName":"Ada"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	trigBody := `{"name":"dual-chat","to":{"subscriberId":"dual_user"},"payload":{},"transactionId":"dc-1"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	// Poll GET /v1/messages until both chat steps have delivered.
	feedURL := srv.URL + "/v1/messages?subscriberId=dual_user&channel=chat&format=array"
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
		sent := 0
		for _, m := range got {
			if m.Status == "sent" {
				sent++
			}
		}
		if sent >= 2 {
			msgs = got
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected 2 chat messages within 20s; last feed JSON: %s", lastBody)
	}

	// --- persisted Message oracle: each step carries its own provider/integration ---
	byStep := map[string]domain.Message{}
	for _, m := range msgs {
		byStep[m.StepID] = m
	}
	c1, ok := byStep["C1"]
	if !ok || c1.Status != "sent" {
		t.Fatalf("C1 message missing or not sent: %+v", byStep)
	}
	if c1.ProviderID != "discord" {
		t.Errorf("C1 ProviderID = %q, want discord", c1.ProviderID)
	}
	c2, ok := byStep["C2"]
	if !ok || c2.Status != "sent" {
		t.Fatalf("C2 message missing or not sent: %+v", byStep)
	}
	if c2.ProviderID != "discord" {
		t.Errorf("C2 ProviderID = %q, want discord", c2.ProviderID)
	}
	if c1.IntegrationID == "" || c2.IntegrationID == "" {
		t.Fatalf("expected non-empty integration ids, got C1=%q C2=%q", c1.IntegrationID, c2.IntegrationID)
	}
	if c1.IntegrationID == c2.IntegrationID {
		t.Errorf("C1 and C2 IntegrationID both %q, want distinct integrations", c1.IntegrationID)
	}

	// --- fake webhook oracle: exactly one POST landed on each ---
	select {
	case <-primaryReceived:
	default:
		t.Error("primary webhook received 0 requests, want 1")
	}
	select {
	case <-primaryReceived:
		t.Error("primary webhook received a second request, want exactly 1")
	default:
	}
	select {
	case <-secondaryReceived:
	default:
		t.Error("secondary webhook received 0 requests, want 1")
	}
	select {
	case <-secondaryReceived:
		t.Error("secondary webhook received a second request, want exactly 1")
	default:
	}
}
