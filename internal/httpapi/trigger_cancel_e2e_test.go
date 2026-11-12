package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// cancelHarness is a real dagnats sidecar, store and HTTP surface with two
// published email workflows: "follow-up" waits cancelDelay before it sends,
// "now" sends at once.
type cancelHarness struct {
	srv        *httptest.Server
	eng        *engine.Engine
	st         *store.Store
	cap        *providers.Capture
	scope      domain.Scope
	authHeader string
	otherAuth  string
}

const cancelDelay = 3 * time.Second

func newCancelHarness(t *testing.T) cancelHarness {
	t.Helper()
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
	eng.Handle("notify.send.email", delivery.NewEmail(st, capResolver{c: cap, from: "noreply@acme.io"}).HandleEmail)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	auth := authoring.New(st, eng)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	otherKey, _, err := id.Bootstrap(ctx, "Other", "dev")
	if err != nil {
		t.Fatalf("Bootstrap other: %v", err)
	}

	email := domain.Step{
		StepID:        "E1",
		Type:          "email",
		ControlValues: json.RawMessage(`{"subject":"Estimate","body":"Still interested?"}`),
	}
	delay := domain.Step{
		StepID:        "d1",
		Type:          "delay",
		ControlValues: json.RawMessage(fmt.Sprintf(`{"amount":%d,"unit":"seconds"}`, int(cancelDelay.Seconds()))),
	}
	for slug, steps := range map[string][]domain.Step{
		"follow-up": {delay, email},
		"now":       {email},
	} {
		if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{Name: slug, TriggerIdentifier: slug, Steps: steps}); err != nil {
			t.Fatalf("authoring.Create %s: %v", slug, err)
		}
		if err := auth.Publish(ctx, scope, slug); err != nil {
			t.Fatalf("authoring.Publish %s: %v", slug, err)
		}
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, eng), Inbox: inboxapi.New(st), Store: st,
	}))
	t.Cleanup(srv.Close)
	h := cancelHarness{
		srv: srv, eng: eng, st: st, cap: cap, scope: scope,
		authHeader: "ApiKey " + key, otherAuth: "ApiKey " + otherKey,
	}
	for _, sub := range []string{"s1", "s2", "s3"} {
		body := fmt.Sprintf(`{"subscriberId":%q,"email":"%s@acme.io"}`, sub, sub)
		if status, rb := doPost(t, srv.URL+"/v1/subscribers", h.authHeader, body); status != http.StatusCreated {
			t.Fatalf("POST /v1/subscribers: %d %s", status, rb)
		}
	}
	return h
}

// trigger fires workflow at to (a JSON "to" object) under tx and returns the
// notification ids.
func (h cancelHarness) trigger(t *testing.T, workflow, to, tx string) []string {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"to":%s,"payload":{},"transactionId":%q}`, workflow, to, tx)
	status, rb := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader, body)
	if status != http.StatusCreated {
		t.Fatalf("trigger %s: %d %s", tx, status, rb)
	}
	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(rb), &resp); err != nil {
		t.Fatalf("decode trigger response: %v", err)
	}
	return resp.NotificationIDs
}

// cancel posts the cancel route and returns the status and the decoded body,
// keyed by its literal wire keys. Like a client, it retries a 503: a run
// started moments ago is not visible in dagnats yet (dagnats#1006).
func (h cancelHarness) cancel(t *testing.T, auth, tx string) (int, map[string][]string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	status, rb := doPost(t, h.srv.URL+"/v1/events/trigger/"+tx+"/cancel", auth, "")
	for status == http.StatusServiceUnavailable && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		status, rb = doPost(t, h.srv.URL+"/v1/events/trigger/"+tx+"/cancel", auth, "")
	}
	if status != http.StatusOK {
		return status, nil
	}
	var wire map[string][]string
	if err := json.Unmarshal([]byte(rb), &wire); err != nil {
		t.Fatalf("decode cancel response %s: %v", rb, err)
	}
	for _, k := range []string{"cancelled", "alreadyFinished"} {
		if _, ok := wire[k]; !ok {
			t.Fatalf("cancel response %s lacks the key %q", rb, k)
		}
	}
	return status, wire
}

func (h cancelHarness) row(t *testing.T, id string) domain.Notification {
	t.Helper()
	n, err := h.st.Notifications().Get(context.Background(), h.scope, id)
	if err != nil {
		t.Fatalf("Get notification %s: %v", id, err)
	}
	return n
}

// runStatus waits for the run's dagnats status to become want.
func (h cancelHarness) waitRun(t *testing.T, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		snap, err := h.eng.GetRun(context.Background(), runID)
		if err == nil && snap.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s status = %q (%v), want %q", runID, snap.Status, err, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h cancelHarness) messages(t *testing.T, notificationID string) []domain.Message {
	t.Helper()
	msgs, err := h.st.Messages().ListByNotificationID(context.Background(), h.scope, notificationID)
	if err != nil {
		t.Fatalf("ListByNotificationID: %v", err)
	}
	return msgs
}

// TestTriggerCancel_E2E is #144 against a real dagnats: a cancel during a delay
// stops the run before its email, is idempotent, reports a finished run as
// such, cancels every recipient of a topic, and is not found for an unknown
// transaction or one in another environment.
func TestTriggerCancel_E2E(t *testing.T) {
	h := newCancelHarness(t)

	ids := h.trigger(t, "follow-up", `{"subscriberId":"s1"}`, "tx-delay")
	if len(ids) != 1 {
		t.Fatalf("notificationIds = %v, want one", ids)
	}
	nid := ids[0]
	runID := h.row(t, nid).RunID
	if runID == "" {
		t.Fatal("row has no run id after the trigger returned")
	}

	status, wire := h.cancel(t, h.authHeader, "tx-delay")
	if status != http.StatusOK || !slices.Equal(wire["cancelled"], []string{nid}) || len(wire["alreadyFinished"]) != 0 {
		t.Fatalf("cancel = %d %v, want 200 cancelled [%s]", status, wire, nid)
	}
	h.waitRun(t, runID, "cancelled")
	if got := h.row(t, nid).Status; got != domain.NotificationCancelled {
		t.Fatalf("notification status = %q, want cancelled", got)
	}

	t.Run("idempotent", func(t *testing.T) {
		status, again := h.cancel(t, h.authHeader, "tx-delay")
		if status != http.StatusOK || !slices.Equal(again["cancelled"], []string{nid}) || len(again["alreadyFinished"]) != 0 {
			t.Fatalf("second cancel = %d %v, want 200 cancelled [%s]", status, again, nid)
		}
	})

	t.Run("unknown transaction", func(t *testing.T) {
		if status, _ := h.cancel(t, h.authHeader, "tx-nope"); status != http.StatusNotFound {
			t.Fatalf("cancel unknown = %d, want 404", status)
		}
	})

	t.Run("another environment", func(t *testing.T) {
		if status, _ := h.cancel(t, h.otherAuth, "tx-delay"); status != http.StatusNotFound {
			t.Fatalf("cancel from another env = %d, want 404", status)
		}
	})

	t.Run("finished run", func(t *testing.T) {
		done := h.trigger(t, "now", `{"subscriberId":"s3"}`, "tx-done")
		h.waitRun(t, h.row(t, done[0]).RunID, "completed")
		status, wire := h.cancel(t, h.authHeader, "tx-done")
		if status != http.StatusOK || len(wire["cancelled"]) != 0 || !slices.Equal(wire["alreadyFinished"], done) {
			t.Fatalf("cancel finished = %d %v, want 200 alreadyFinished %v", status, wire, done)
		}
		if got := h.row(t, done[0]).Status; got != domain.NotificationStarted {
			t.Fatalf("finished notification status = %q, want started", got)
		}
	})

	t.Run("topic fan-out", func(t *testing.T) {
		if status, rb := doPost(t, h.srv.URL+"/v1/topics", h.authHeader, `{"key":"crew","name":"Crew"}`); status != http.StatusCreated {
			t.Fatalf("POST /v1/topics: %d %s", status, rb)
		}
		for _, sub := range []string{"s1", "s2"} {
			status, rb := doPost(t, h.srv.URL+"/v1/topics/crew/subscribers", h.authHeader, fmt.Sprintf(`{"subscriberId":%q}`, sub))
			if status != http.StatusNoContent && status != http.StatusCreated {
				t.Fatalf("add %s to topic: %d %s", sub, status, rb)
			}
		}
		fan := h.trigger(t, "follow-up", `{"topicKey":"crew"}`, "tx-topic")
		if len(fan) != 2 {
			t.Fatalf("topic notificationIds = %v, want two", fan)
		}
		status, wire := h.cancel(t, h.authHeader, "tx-topic")
		got := slices.Sorted(slices.Values(wire["cancelled"]))
		if status != http.StatusOK || !slices.Equal(got, slices.Sorted(slices.Values(fan))) {
			t.Fatalf("cancel topic = %d %v, want 200 cancelled %v", status, wire, fan)
		}
		for _, id := range fan {
			h.waitRun(t, h.row(t, id).RunID, "cancelled")
		}
	})

	// Past every cancelled run's delay: none of them sent its email. The only
	// email is the "now" workflow's.
	time.Sleep(cancelDelay + 2*time.Second)
	if msgs := h.messages(t, nid); len(msgs) != 0 {
		t.Fatalf("cancelled notification has messages %+v, want none", msgs)
	}
	for _, m := range h.cap.Sent() {
		if m.Subject == "Estimate" && m.To != "s3@acme.io" {
			t.Fatalf("an email went to %s after its transaction was cancelled", m.To)
		}
	}
	if n := len(h.cap.Sent()); n != 1 {
		t.Fatalf("emails sent = %d, want 1 (the finished run's)", n)
	}
}
