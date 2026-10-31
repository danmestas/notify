package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// TestRealtime_InAppStreamsToSubscriber is the live SSE proof: a real dagnats
// sidecar, real temp SQLite, and the real HTTP surface (including the realtime
// gateway) drive subscriber -> trigger -> delivered in_app message and assert
// the message is pushed over a live SSE stream to the subscriber.
func TestRealtime_InAppStreamsToSubscriber(t *testing.T) {
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

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	// Register the in_app delivery handler with a live publisher before workers start.
	eng.Handle("send.in_app", delivery.New(st).WithEventBus(bus).HandleInApp)
	eng.StartWorkers(ctx)

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)
	rt := realtime.New(bus)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

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

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Realtime: rt}))
	defer srv.Close()

	authHeader := "ApiKey " + key

	// Create the subscriber.
	subBody := `{"subscriberId":"rt_user","firstName":"Ada"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// Open the live SSE stream with a cancelable context.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, srv.URL+"/v1/inbox/stream?subscriberId=rt_user", nil)
	if err != nil {
		t.Fatalf("new stream request: %v", err)
	}
	streamReq.Header.Set("Authorization", authHeader)
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("SSE stream status: %d", streamResp.StatusCode)
	}
	defer func() { _ = streamResp.Body.Close() }()

	frames := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(streamResp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				select {
				case frames <- strings.TrimPrefix(line, "data: "):
				default:
				}
			}
		}
	}()

	// Give the SSE subscription a moment to establish on the bus before triggering.
	time.Sleep(300 * time.Millisecond)

	trigBody := `{"name":"welcome","to":{"subscriberId":"rt_user"},"payload":{"itemCount":3},"transactionId":"rt-1"}`
	if status, body := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, body)
	}

	var payload string
	select {
	case payload = <-frames:
	case <-time.After(8 * time.Second):
		t.Fatalf("no SSE 'data: ' frame arrived within 8s")
	}

	t.Logf("SSE frame: %s", payload)

	var msg domain.Message
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		t.Fatalf("decode SSE message: %v (payload=%s)", err, payload)
	}
	if msg.Channel != "in_app" {
		t.Errorf("Channel = %q, want %q", msg.Channel, "in_app")
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

	// Disconnect the live stream.
	cancelStream()
}
