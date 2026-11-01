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
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// capResolver is a tiny in-test EmailResolver that returns a Capture provider
// and a fixed From address. No mocks: Capture is the real recording provider.
type capResolver struct {
	c    *providers.Capture
	from string
}

func (r capResolver) ResolveEmail(_ context.Context, _ domain.Scope) (providers.EmailProvider, string, string, string, error) {
	return r.c, r.from, "smtp", "", nil
}

func (r capResolver) ResolveEmailIntegration(_ context.Context, _ domain.Scope, identifier string) (providers.EmailProvider, string, string, string, error) {
	return r.c, r.from, "smtp", "", nil
}

// TestEmailParity_EndToEnd is the email slice parity proof: a real dagnats
// sidecar, a real temp SQLite store, and the real HTTP surface drive the full
// scenario (subscriber -> trigger -> delivered email message) and
// assert both the persisted Message and the captured provider invocation.
func TestEmailParity_EndToEnd(t *testing.T) {
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

	cap := providers.NewCapture()

	// Register both delivery handlers before workers start.
	eng.Handle("notify.send.in_app", delivery.New(st).HandleInApp)
	eng.Handle("notify.send.email", delivery.NewEmail(st, capResolver{c: cap, from: "noreply@acme.io"}).HandleEmail)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	masterKey := bytesRepeat(0x2a, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	trg := trigger.New(aud, st, eng)
	inbox := inboxapi.New(st)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Author + publish a 1-step email workflow via the service (not HTTP).
	// htmlBody rides the real trigger path into the envelope (#369): the
	// captured email must carry both parts.
	controls := json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items","htmlBody":"<p>You have <b>{{payload.itemCount}}</b> items</p>"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome Email",
		TriggerIdentifier: "welcome-email",
		Steps: []domain.Step{{
			StepID:        "E1",
			Type:          "email",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "welcome-email"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox}))
	defer srv.Close()

	authHeader := "ApiKey " + key

	// POST /v1/subscribers -> 201.
	subBody := `{"subscriberId":"user_9","firstName":"Ada","email":"ada@acme.io"}`
	if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, subBody); status != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", status, body)
	}

	// POST /v1/events/trigger -> 201 + TriggerResponse.
	trigBody := `{"name":"welcome-email","to":{"subscriberId":"user_9"},"payload":{"itemCount":3},"transactionId":"etx-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}
	t.Logf("trigger response: %s", respJSON)

	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	if !resp.Acknowledged {
		t.Errorf("Acknowledged = false, want true")
	}
	if resp.Status != "processed" {
		t.Errorf("Status = %q, want %q", resp.Status, "processed")
	}
	if resp.TransactionID != "etx-1" {
		t.Errorf("TransactionID = %q, want %q", resp.TransactionID, "etx-1")
	}

	// Poll GET /v1/messages until the delivered email message arrives.
	feedURL := srv.URL + "/v1/messages?subscriberId=user_9&channel=email&format=array"
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
		t.Logf("poll: 0 messages yet (feed=%s)", body)
		time.Sleep(300 * time.Millisecond)
	}
	if len(msgs) == 0 {
		t.Fatalf("no email message arrived within 20s; last feed JSON: %s", lastBody)
	}

	msg := msgs[0]
	msgJSON, _ := json.Marshal(msg)
	t.Logf("message: %s", msgJSON)

	// --- persisted Message oracle ---
	if msg.Channel != "email" {
		t.Errorf("Channel = %q, want %q", msg.Channel, "email")
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
	if msg.TransactionID != "etx-1" {
		t.Errorf("TransactionID = %q, want %q", msg.TransactionID, "etx-1")
	}

	// --- Capture oracle: the provider was actually invoked exactly once ---
	sent := cap.Sent()
	if len(sent) != 1 {
		t.Fatalf("cap.Sent() len = %d, want 1 (%+v)", len(sent), sent)
	}
	em := sent[0]
	t.Logf("captured email: %+v", em)
	if em.To != "ada@acme.io" {
		t.Errorf("captured To = %q, want %q", em.To, "ada@acme.io")
	}
	if em.From != "noreply@acme.io" {
		t.Errorf("captured From = %q, want %q", em.From, "noreply@acme.io")
	}
	if em.Subject != "Hi Ada" {
		t.Errorf("captured Subject = %q, want %q", em.Subject, "Hi Ada")
	}
	if em.Text != "You have 3 items" {
		t.Errorf("captured Text = %q, want %q", em.Text, "You have 3 items")
	}
	if em.HTML != "<p>You have <b>3</b> items</p>" {
		t.Errorf("captured HTML = %q, want the htmlBody part", em.HTML)
	}
}

func bytesRepeat(b byte, n int) []byte {
	s := make([]byte, n)
	for i := range s {
		s[i] = b
	}
	return s
}
