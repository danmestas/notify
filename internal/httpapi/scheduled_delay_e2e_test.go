package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
)

// TestScheduledDelay_FiresInTheSubscribersTimezone: a scheduled delay is a
// cron sleep the engine resolves in the recipient's timezone. The subscriber
// is in Tokyo and the delay is set for the next minute there. In UTC that
// time is hours away, so the message arriving within two minutes proves the
// engine used the subscriber's zone. The harness registers no delay.scheduled
// handler, so nothing else could have done the waiting.
func TestScheduledDelay_FiresInTheSubscribersTimezone(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a wall-clock minute")
	}
	h := newHTTPActionHarness(t)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	status, body := doPost(t, h.srv.URL+"/v1/subscribers", h.authHeader, `{"subscriberId":"sd_tokyo"}`)
	var created struct {
		ID string `json:"id"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &created) != nil || created.ID == "" {
		t.Fatalf("create subscriber: %d %s", status, body)
	}
	// PATCH takes the subscriber's row id, and is where a timezone is set.
	req, _ := http.NewRequest(http.MethodPatch, h.srv.URL+"/v1/subscribers/"+created.ID, strings.NewReader(`{"timezone":"Asia/Tokyo"}`))
	req.Header.Set("Authorization", h.authHeader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("set timezone: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	fire := time.Now().In(tokyo).Truncate(time.Minute).Add(time.Minute)
	cv, _ := json.Marshal(map[string]any{"type": "scheduled", "atTime": fire.Format("15:04")})
	if _, err := h.auth.Create(h.ctx, h.scope, authoring.WorkflowInput{
		Name: "tokyo-morning", TriggerIdentifier: "tokyo-morning",
		Steps: []domain.Step{
			{StepID: "d1", Type: "delay", ControlValues: cv},
			{StepID: "s1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"S","body":"on time in Tokyo"}`)},
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.auth.Publish(h.ctx, h.scope, "tokyo-morning"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if status, body := doPost(t, h.srv.URL+"/v1/events/trigger", h.authHeader,
		`{"name":"tokyo-morning","to":{"subscriberId":"sd_tokyo"},"payload":{},"transactionId":"tx-tokyo"}`); status != http.StatusCreated {
		t.Fatalf("trigger: %d %s", status, body)
	}

	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		_, body := doGet(t, h.srv.URL+"/v1/messages?subscriberId=sd_tokyo&channel=in_app&format=array", h.authHeader)
		var got []domain.Message
		_ = json.Unmarshal([]byte(body), &got)
		if len(got) > 0 {
			if early := time.Until(fire); early > time.Second {
				t.Fatalf("message arrived %v before the scheduled minute", early)
			}
			if got[0].Content != "on time in Tokyo" {
				t.Fatalf("message = %+v", got[0])
			}
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no message by %s Tokyo time: the delay did not fire in the subscriber's timezone", fire.Format("15:04"))
}
