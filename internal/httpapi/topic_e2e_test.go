package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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

// TestTopic_MulticastDelivers is the multicast parity proof: a single trigger
// addressed to a topic (to.topicKey) fans out to every member, and each member
// receives a correctly rendered in_app message through the real engine +
// delivery worker. Both s1 ("Hi Ada") and s2 ("Hi Bob") must get the broadcast.
func TestTopic_MulticastDelivers(t *testing.T) {
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

	controls := json.RawMessage(`{"subject":"Hi {{subscriber.firstName}}","body":"You have {{payload.itemCount}} items"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Broadcast",
		TriggerIdentifier: "broadcast",
		Steps: []domain.Step{{
			StepID:        "S1",
			Type:          "in_app",
			ControlValues: controls,
		}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "broadcast"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, aud, trg, inbox, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	defer srv.Close()

	authHeader := "ApiKey " + key

	// Two subscribers with distinct first names so rendering is per-recipient.
	for _, sub := range []string{
		`{"subscriberId":"s1","firstName":"Ada"}`,
		`{"subscriberId":"s2","firstName":"Bob"}`,
	} {
		if status, body := doPost(t, srv.URL+"/v1/subscribers", authHeader, sub); status != http.StatusCreated {
			t.Fatalf("POST /v1/subscribers %s: status=%d body=%s", sub, status, body)
		}
	}

	// Create the topic and add both subscribers to it.
	if status, body := doPost(t, srv.URL+"/v1/topics", authHeader, `{"key":"team","name":"Team"}`); status != http.StatusCreated {
		t.Fatalf("POST /v1/topics: status=%d body=%s", status, body)
	}
	for _, id := range []string{"s1", "s2"} {
		body := fmt.Sprintf(`{"subscriberId":%q}`, id)
		status, respBody := doPost(t, srv.URL+"/v1/topics/team/subscribers", authHeader, body)
		if status != http.StatusNoContent && status != http.StatusCreated {
			t.Fatalf("POST /v1/topics/team/subscribers %s: status=%d body=%s", id, status, respBody)
		}
	}

	// One multicast trigger to the topic.
	trigBody := `{"name":"broadcast","to":{"topicKey":"team"},"payload":{"itemCount":3},"transactionId":"bc-1"}`
	status, respJSON := doPost(t, srv.URL+"/v1/events/trigger", authHeader, trigBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/events/trigger: status=%d body=%s", status, respJSON)
	}
	t.Logf("trigger response: %s", respJSON)

	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("decode TriggerResponse: %v (body=%s)", err, respJSON)
	}
	if !resp.Acknowledged || resp.Status != "processed" || resp.TransactionID != "bc-1" {
		t.Fatalf("trigger resp = %+v, want acknowledged/processed/bc-1", resp)
	}

	// Both members must receive the same broadcast, each rendered for itself.
	ada := pollMessage(t, srv.URL, authHeader, "s1")
	bob := pollMessage(t, srv.URL, authHeader, "s2")

	adaJSON, _ := json.Marshal(ada)
	bobJSON, _ := json.Marshal(bob)
	t.Logf("s1 message: %s", adaJSON)
	t.Logf("s2 message: %s", bobJSON)

	if ada.Subject != "Hi Ada" {
		t.Errorf("s1 Subject = %q, want %q", ada.Subject, "Hi Ada")
	}
	if bob.Subject != "Hi Bob" {
		t.Errorf("s2 Subject = %q, want %q", bob.Subject, "Hi Bob")
	}
	for _, m := range []domain.Message{ada, bob} {
		if m.Channel != "in_app" {
			t.Errorf("%s Channel = %q, want in_app", m.SubscriberID, m.Channel)
		}
		if m.Content != "You have 3 items" {
			t.Errorf("%s Content = %q, want %q", m.SubscriberID, m.Content, "You have 3 items")
		}
		if m.Status != "sent" {
			t.Errorf("%s Status = %q, want sent", m.SubscriberID, m.Status)
		}
		if m.TransactionID != "bc-1" {
			t.Errorf("%s TransactionID = %q, want bc-1", m.SubscriberID, m.TransactionID)
		}
	}
}
